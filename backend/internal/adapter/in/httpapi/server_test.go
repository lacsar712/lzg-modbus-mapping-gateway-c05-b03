package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/bytecode/modbus-mapping-gateway/internal/domain"
	"github.com/bytecode/modbus-mapping-gateway/internal/usecase"
)

type fakeStore struct{ cfg domain.MappingConfig }

func (f *fakeStore) Load() (domain.MappingConfig, string, error) { return f.cfg, "", nil }
func (f *fakeStore) Save(string) error                           { return nil }
func (f *fakeStore) Path() string                                { return "fake" }

type fakeModbus struct {
	mu   sync.Mutex
	regs map[uint16]uint16
}

func (f *fakeModbus) ReadHoldingRegisters(endpoint string, unitID byte, timeoutMs int, address, quantity uint16) ([]uint16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uint16, quantity)
	for i := range out {
		out[i] = f.regs[address+uint16(i)]
	}
	return out, nil
}

func (f *fakeModbus) WriteSingleRegister(endpoint string, unitID byte, timeoutMs int, address, value uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regs[address] = value
	return nil
}

func (f *fakeModbus) WriteMultipleRegisters(endpoint string, unitID byte, timeoutMs int, address uint16, values []uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, v := range values {
		f.regs[address+uint16(i)] = v
	}
	return nil
}

func (f *fakeModbus) reg(address uint16) uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.regs[address]
}

func testConfig() domain.MappingConfig {
	bit := 0
	minRPM, maxRPM := 0.0, 5000.0
	return domain.MappingConfig{Devices: []domain.DeviceDef{{
		ID: "plc-line-a", Name: "test plc", Endpoint: "fake:502", UnitID: 1, TimeoutMs: 100,
		Points: []domain.PointDef{
			{Name: "motor_rpm", Address: 0, Type: domain.TypeFloat32ABCD, Writable: true, Scale: 1, Min: &minRPM, Max: &maxRPM},
			{Name: "run_flag", Address: 40, Type: domain.TypeBoolBit, Bit: &bit, Writable: true, Scale: 1},
		},
	}}}
}

func setupServer(t *testing.T) (*gin.Engine, *fakeModbus) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fm := &fakeModbus{regs: map[uint16]uint16{40: 0x00A5}}
	svc, err := usecase.NewGatewayService(&fakeStore{cfg: testConfig()}, fm)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(svc).Router(), fm
}

func login(t *testing.T, r *gin.Engine, username, password string) string {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", username, w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Token
}

func doJSON(t *testing.T, r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w
}

type previewResponse struct {
	OK           bool     `json:"ok"`
	Violations   []string `json:"violations"`
	Raw          float64  `json:"raw"`
	Registers    []uint16 `json:"registers"`
	PreviewToken string   `json:"previewToken"`
	ExpiresAt    string   `json:"expiresAt"`
	Mask         *struct {
		Existing  uint16 `json:"existing"`
		Preserved uint16 `json:"preserved"`
		Merged    uint16 `json:"merged"`
	} `json:"mask"`
}

func doPreview(t *testing.T, r *gin.Engine, token, point string, value float64) previewResponse {
	t.Helper()
	w := doJSON(t, r, http.MethodPost, "/api/devices/plc-line-a/points/"+point+"/preview", token,
		fmt.Sprintf(`{"value":%v}`, value))
	if w.Code != http.StatusOK {
		t.Fatalf("preview %s: %d %s", point, w.Code, w.Body.String())
	}
	var prev previewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &prev); err != nil {
		t.Fatal(err)
	}
	return prev
}

// observer 可预演不可提交：preview 200，PUT 403（即使带着合法令牌）。
func TestObserverCanPreviewButNotSubmit(t *testing.T) {
	r, fm := setupServer(t)
	tok := login(t, r, "observer", "obs123456")

	prev := doPreview(t, r, tok, "run_flag", 1)
	if !prev.OK || prev.PreviewToken == "" || prev.ExpiresAt == "" {
		t.Fatalf("observer preview got %+v", prev)
	}
	if prev.Mask == nil || prev.Mask.Existing != 0x00A5 {
		t.Fatalf("observer mask got %+v", prev.Mask)
	}

	w := doJSON(t, r, http.MethodPut, "/api/devices/plc-line-a/points/run_flag", tok,
		fmt.Sprintf(`{"value":1,"previewToken":%q}`, prev.PreviewToken))
	if w.Code != http.StatusForbidden {
		t.Fatalf("observer PUT: got %d want 403", w.Code)
	}
	if fm.reg(40) != 0x00A5 {
		t.Fatal("observer PUT must not reach the bus")
	}
}

// engineer 可走完预演加提交：bool_bit 无令牌 400，有令牌 200 且保位，令牌一次性。
func TestEngineerPreviewThenSubmitBoolBit(t *testing.T) {
	r, fm := setupServer(t)
	tok := login(t, r, "engineer", "mod123456")

	w := doJSON(t, r, http.MethodPut, "/api/devices/plc-line-a/points/run_flag", tok, `{"value":0}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT without token: got %d want 400", w.Code)
	}

	prev := doPreview(t, r, tok, "run_flag", 0)
	if prev.Mask == nil || prev.Mask.Preserved != 0x00A4 || prev.Mask.Merged != 0x00A4 {
		t.Fatalf("mask got %+v", prev.Mask)
	}

	w = doJSON(t, r, http.MethodPut, "/api/devices/plc-line-a/points/run_flag", tok,
		fmt.Sprintf(`{"value":0,"previewToken":%q}`, prev.PreviewToken))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT with token: %d %s", w.Code, w.Body.String())
	}
	if fm.reg(40) != 0x00A4 {
		t.Fatalf("reg got %#04x want 0x00a4 (other bits preserved)", fm.reg(40))
	}

	w = doJSON(t, r, http.MethodPut, "/api/devices/plc-line-a/points/run_flag", tok,
		fmt.Sprintf(`{"value":0,"previewToken":%q}`, prev.PreviewToken))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("replayed token: got %d want 400", w.Code)
	}
}

// 越界：无令牌 400；有令牌仍 400（后端再校验 min/max）。范围内标量写保持免令牌。
func TestOutOfRangeGateAndRevalidation(t *testing.T) {
	r, _ := setupServer(t)
	tok := login(t, r, "engineer", "mod123456")

	w := doJSON(t, r, http.MethodPut, "/api/devices/plc-line-a/points/motor_rpm", tok, `{"value":1800}`)
	if w.Code != http.StatusOK {
		t.Fatalf("in-range PUT: %d %s", w.Code, w.Body.String())
	}

	w = doJSON(t, r, http.MethodPut, "/api/devices/plc-line-a/points/motor_rpm", tok, `{"value":6000}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range without token: got %d want 400", w.Code)
	}

	prev := doPreview(t, r, tok, "motor_rpm", 6000)
	if prev.OK || len(prev.Violations) == 0 {
		t.Fatalf("expected violations, got %+v", prev)
	}
	w = doJSON(t, r, http.MethodPut, "/api/devices/plc-line-a/points/motor_rpm", tok,
		fmt.Sprintf(`{"value":6000,"previewToken":%q}`, prev.PreviewToken))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range with token: got %d want 400", w.Code)
	}
}

// 摘要不一致：value=1 预演的令牌不能用于 value=0 的 PUT。
func TestTokenDigestMismatchOverHTTP(t *testing.T) {
	r, _ := setupServer(t)
	tok := login(t, r, "engineer", "mod123456")

	prev := doPreview(t, r, tok, "run_flag", 1)
	w := doJSON(t, r, http.MethodPut, "/api/devices/plc-line-a/points/run_flag", tok,
		fmt.Sprintf(`{"value":0,"previewToken":%q}`, prev.PreviewToken))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("digest mismatch: got %d want 400", w.Code)
	}
}
