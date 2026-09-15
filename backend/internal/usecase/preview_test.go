package usecase

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeModbus is an in-memory port.ModbusClient for tests.
type fakeModbus struct {
	mu        sync.Mutex
	regs      map[uint16]uint16
	readErr   error
	writeErr  error
	lastWrite *writeCall
}

type writeCall struct {
	address uint16
	values  []uint16
	single  bool
}

func (f *fakeModbus) ReadHoldingRegisters(endpoint string, unitID byte, timeoutMs int, address, quantity uint16) ([]uint16, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uint16, quantity)
	for i := range out {
		out[i] = f.regs[address+uint16(i)]
	}
	return out, nil
}

func (f *fakeModbus) WriteSingleRegister(endpoint string, unitID byte, timeoutMs int, address, value uint16) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regs[address] = value
	f.lastWrite = &writeCall{address: address, values: []uint16{value}, single: true}
	return nil
}

func (f *fakeModbus) WriteMultipleRegisters(endpoint string, unitID byte, timeoutMs int, address uint16, values []uint16) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, v := range values {
		f.regs[address+uint16(i)] = v
	}
	f.lastWrite = &writeCall{address: address, values: append([]uint16(nil), values...)}
	return nil
}

const testMapping = `
devices:
  - id: plc-line-a
    name: test plc
    endpoint: fake:502
    unitId: 1
    timeoutMs: 100
    points:
      - name: motor_rpm
        address: 0
        type: float32_abcd
        writable: true
        scale: 1
        offset: 0
        min: 0
        max: 5000
      - name: pressure
        address: 20
        type: int16
        writable: true
        scale: 0.1
        offset: 0
        min: 0
        max: 50
      - name: status_word
        address: 30
        type: uint16
        writable: false
        scale: 1
        offset: 0
      - name: run_flag
        address: 40
        type: bool_bit
        bit: 0
        writable: true
        scale: 1
        offset: 0
`

func newTestService(t *testing.T, fm *fakeModbus) *GatewayService {
	t.Helper()
	svc, err := NewGatewayService(&memStore{text: testMapping}, fm)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// pressure: eng 12.5 with scale 0.1 must invert to raw 125 and encode to [125].
func TestPreviewPressureInverse(t *testing.T) {
	svc := newTestService(t, &fakeModbus{regs: map[uint16]uint16{}})

	res, err := svc.PreviewWrite("engineer", "plc-line-a", "pressure", 12.5)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(res.Violations) != 0 {
		t.Fatalf("expected ok preview, got %+v", res)
	}
	if math.Abs(res.Raw-125) > 1e-9 {
		t.Fatalf("raw got %v want 125", res.Raw)
	}
	if len(res.Registers) != 1 || res.Registers[0] != 125 {
		t.Fatalf("registers got %v want [125]", res.Registers)
	}
	if res.WriteFunction != "single" {
		t.Fatalf("writeFunction got %q", res.WriteFunction)
	}
	if res.PreviewToken == "" {
		t.Fatal("expected preview token")
	}
	if res.TTLSeconds != int(defaultPreviewTTL.Seconds()) {
		t.Fatalf("ttl got %d", res.TTLSeconds)
	}
	exp, err := time.Parse(time.RFC3339, res.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt parse: %v", err)
	}
	if !exp.After(time.Now()) {
		t.Fatalf("expiresAt %v not in the future", exp)
	}
}

// motor_rpm=6000 exceeds max 5000: preview reports the violation, and the
// formal PUT is rejected with or without a token.
func TestPreviewMotorRpmOutOfRange(t *testing.T) {
	fm := &fakeModbus{regs: map[uint16]uint16{}}
	svc := newTestService(t, fm)

	res, err := svc.PreviewWrite("engineer", "plc-line-a", "motor_rpm", 6000)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("expected violations for out-of-range preview")
	}
	if len(res.Violations) == 0 || !strings.Contains(res.Violations[0], "above max") {
		t.Fatalf("violations got %v", res.Violations)
	}

	// out-of-range PUT without a token: rejected at the token gate
	err = svc.WritePoint("engineer", "plc-line-a", "motor_rpm", 6000, "")
	if err == nil || !strings.Contains(err.Error(), "preview token required") {
		t.Fatalf("expected token-required error, got %v", err)
	}

	// even with a valid token the backend re-validates min/max
	err = svc.WritePoint("engineer", "plc-line-a", "motor_rpm", 6000, res.PreviewToken)
	if err == nil || !strings.Contains(err.Error(), "above max") {
		t.Fatalf("expected min/max rejection, got %v", err)
	}
	if fm.lastWrite != nil {
		t.Fatal("no bus write expected for rejected value")
	}
}

// run_flag (bool_bit bit0): read-modify-write must keep the other bits of the
// register, the token is one-time, and preview must not touch the bus.
func TestRunFlagPreservesOtherBits(t *testing.T) {
	fm := &fakeModbus{regs: map[uint16]uint16{40: 0x00A4}}
	svc := newTestService(t, fm)

	res, err := svc.PreviewWrite("engineer", "plc-line-a", "run_flag", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("preview not ok: %+v", res)
	}
	if res.Mask == nil {
		t.Fatal("expected mask info for bool_bit")
	}
	if res.Mask.Existing != 0x00A4 || res.Mask.Mask != 0x0001 ||
		res.Mask.Preserved != 0x00A4 || res.Mask.Merged != 0x00A5 {
		t.Fatalf("mask got %+v", res.Mask)
	}
	if len(res.Registers) != 1 || res.Registers[0] != 0x00A5 {
		t.Fatalf("registers got %v want [0x00a5]", res.Registers)
	}
	if fm.lastWrite != nil || fm.regs[40] != 0x00A4 {
		t.Fatal("preview must not write to the bus")
	}

	// bool_bit PUT without token is rejected
	if err := svc.WritePoint("engineer", "plc-line-a", "run_flag", 1, ""); err == nil {
		t.Fatal("expected token-required error for bool_bit write")
	}

	// with the preview token the write goes through and keeps other bits
	if err := svc.WritePoint("engineer", "plc-line-a", "run_flag", 1, res.PreviewToken); err != nil {
		t.Fatal(err)
	}
	if fm.lastWrite == nil || !fm.lastWrite.single || fm.lastWrite.address != 40 || fm.lastWrite.values[0] != 0x00A5 {
		t.Fatalf("write got %+v", fm.lastWrite)
	}
	if fm.regs[40]&0x00A4 != 0x00A4 {
		t.Fatalf("other bits lost: %#04x", fm.regs[40])
	}

	// token is one-time: replay must fail
	if err := svc.WritePoint("engineer", "plc-line-a", "run_flag", 1, res.PreviewToken); err == nil {
		t.Fatal("expected one-time token rejection")
	}

	// clearing the flag keeps the other bits too
	res2, err := svc.PreviewWrite("engineer", "plc-line-a", "run_flag", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Mask == nil || res2.Mask.Merged != 0x00A4 {
		t.Fatalf("clear mask got %+v", res2.Mask)
	}
	if err := svc.WritePoint("engineer", "plc-line-a", "run_flag", 0, res2.PreviewToken); err != nil {
		t.Fatal(err)
	}
	if fm.regs[40] != 0x00A4 {
		t.Fatalf("clear got %#04x want 0x00a4", fm.regs[40])
	}
}

// An expired preview token must be rejected.
func TestExpiredPreviewTokenRejected(t *testing.T) {
	fm := &fakeModbus{regs: map[uint16]uint16{40: 0x0001}}
	svc := newTestService(t, fm)
	svc.previewTTL = -time.Second // tokens are already expired when issued

	res, err := svc.PreviewWrite("engineer", "plc-line-a", "run_flag", 1)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.WritePoint("engineer", "plc-line-a", "run_flag", 1, res.PreviewToken)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired-token rejection, got %v", err)
	}
	if fm.lastWrite != nil {
		t.Fatal("no bus write expected with expired token")
	}
}

// A token is bound to the exact request body: value=1's token must not
// authorize a PUT of value=0.
func TestPreviewTokenDigestMismatch(t *testing.T) {
	fm := &fakeModbus{regs: map[uint16]uint16{40: 0x0000}}
	svc := newTestService(t, fm)

	res, err := svc.PreviewWrite("engineer", "plc-line-a", "run_flag", 1)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.WritePoint("engineer", "plc-line-a", "run_flag", 0, res.PreviewToken)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected digest-mismatch rejection, got %v", err)
	}
	if fm.lastWrite != nil {
		t.Fatal("no bus write expected on digest mismatch")
	}
}

// In-range scalar writes stay token-free (backwards compatible).
func TestScalarWriteWithoutTokenStillWorks(t *testing.T) {
	fm := &fakeModbus{regs: map[uint16]uint16{}}
	svc := newTestService(t, fm)

	if err := svc.WritePoint("engineer", "plc-line-a", "pressure", 12.5, ""); err != nil {
		t.Fatal(err)
	}
	if fm.regs[20] != 125 {
		t.Fatalf("reg got %d want 125", fm.regs[20])
	}
}

// Read-only points cannot be previewed.
func TestPreviewReadOnlyPointRejected(t *testing.T) {
	svc := newTestService(t, &fakeModbus{regs: map[uint16]uint16{}})

	_, err := svc.PreviewWrite("engineer", "plc-line-a", "status_word", 1)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only error, got %v", err)
	}
}

// A token issued to one user must not be usable by another.
func TestPreviewTokenBoundToUser(t *testing.T) {
	fm := &fakeModbus{regs: map[uint16]uint16{40: 0x0000}}
	svc := newTestService(t, fm)

	res, err := svc.PreviewWrite("engineer", "plc-line-a", "run_flag", 1)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.WritePoint("other-user", "plc-line-a", "run_flag", 1, res.PreviewToken)
	if err == nil || !strings.Contains(err.Error(), "another user") {
		t.Fatalf("expected user-binding rejection, got %v", err)
	}
}

// Preview of a bool_bit point surfaces a bus read failure as an error.
func TestPreviewBoolBitReadFailure(t *testing.T) {
	readErr := errors.New("dial timeout")
	svc := newTestService(t, &fakeModbus{regs: map[uint16]uint16{}, readErr: readErr})

	_, err := svc.PreviewWrite("engineer", "plc-line-a", "run_flag", 1)
	if err == nil || !strings.Contains(err.Error(), "dial timeout") {
		t.Fatalf("expected read failure, got %v", err)
	}
}
