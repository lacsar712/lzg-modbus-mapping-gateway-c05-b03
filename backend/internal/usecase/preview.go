package usecase

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/bytecode/modbus-mapping-gateway/internal/domain"
)

// defaultPreviewTTL is the lifetime of a one-time preview confirmation token.
const defaultPreviewTTL = 60 * time.Second

// MaskInfo describes the bool_bit read-modify-write key points of a preview.
type MaskInfo struct {
	Address   uint16 `json:"address"`
	Bit       int    `json:"bit"`
	Mask      uint16 `json:"mask"`
	Existing  uint16 `json:"existing"`
	Preserved uint16 `json:"preserved"`
	Merged    uint16 `json:"merged"`
}

// PreviewResult is the outcome of a dry-run write: same rules as the real
// write path (CheckMinMax -> InvertScale -> bool_bit read -> EncodeRegisters)
// but nothing is written to the bus.
type PreviewResult struct {
	DeviceID      string    `json:"deviceId"`
	Point         string    `json:"point"`
	Value         float64   `json:"value"`
	OK            bool      `json:"ok"`
	Violations    []string  `json:"violations"`
	Raw           float64   `json:"raw"`
	Registers     []uint16  `json:"registers"`
	Mask          *MaskInfo `json:"mask,omitempty"`
	WriteFunction string    `json:"writeFunction,omitempty"`
	PreviewToken  string    `json:"previewToken"`
	ExpiresAt     string    `json:"expiresAt"`
	TTLSeconds    int       `json:"ttlSeconds"`
}

// previewDigest binds a confirmation token to one exact request body:
// device + point + engineering value.
func previewDigest(deviceID, name string, value float64) string {
	sum := sha256.Sum256([]byte(deviceID + "|" + name + "|" + strconv.FormatFloat(value, 'g', -1, 64)))
	return hex.EncodeToString(sum[:])
}

type previewTokenRecord struct {
	username  string
	digest    string
	expiresAt time.Time
}

// previewStore keeps one-time preview tokens in memory.
type previewStore struct {
	mu     sync.Mutex
	tokens map[string]*previewTokenRecord
}

func newPreviewStore() *previewStore {
	return &previewStore{tokens: map[string]*previewTokenRecord{}}
}

// issue creates a token for (username, digest) and lazily sweeps expired ones.
func (ps *previewStore) issue(username, digest string, ttl time.Duration) (string, time.Time) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Errorf("token rand: %w", err))
	}
	token := hex.EncodeToString(buf[:])
	now := time.Now()
	exp := now.Add(ttl)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for k, v := range ps.tokens {
		if now.After(v.expiresAt) {
			delete(ps.tokens, k)
		}
	}
	ps.tokens[token] = &previewTokenRecord{username: username, digest: digest, expiresAt: exp}
	return token, exp
}

// validate checks that a token exists, is unexpired, and matches the user and
// the digest of the request body. It does NOT consume the token; a successful
// write consumes it via burn.
func (ps *previewStore) validate(username, digest, token string) error {
	if token == "" {
		return fmt.Errorf("preview token required: run preview first")
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	rec, ok := ps.tokens[token]
	if !ok {
		return fmt.Errorf("preview token invalid or already used")
	}
	if time.Now().After(rec.expiresAt) {
		return fmt.Errorf("preview token expired")
	}
	if rec.username != username {
		return fmt.Errorf("preview token issued to another user")
	}
	if rec.digest != digest {
		return fmt.Errorf("preview token does not match request body")
	}
	return nil
}

// burn invalidates a token after a legitimate write. Empty token is a no-op.
func (ps *previewStore) burn(token string) {
	if token == "" {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.tokens, token)
}

// PreviewWrite dry-runs a write through the exact same rule chain as
// WritePoint (CheckMinMax -> InvertScale -> bool_bit read-modify-write ->
// EncodeRegisters) without touching the bus write. It returns the raw value,
// the registers that would be written, bool_bit mask key points, and a
// one-time short-lived previewToken that a formal PUT must present for
// out-of-range or bool_bit writes.
func (s *GatewayService) PreviewWrite(username, deviceID, name string, engValue float64) (*PreviewResult, error) {
	d, p, err := s.GetPoint(deviceID, name)
	if err != nil {
		return nil, err
	}
	if !p.Writable {
		return nil, fmt.Errorf("point %s is read-only", name)
	}

	res := &PreviewResult{
		DeviceID:   deviceID,
		Point:      name,
		Value:      engValue,
		OK:         true,
		Violations: []string{},
	}

	// Same rule order as WritePoint: range check first, then inverse scale.
	if err := domain.CheckMinMax(engValue, p.Min, p.Max); err != nil {
		res.OK = false
		res.Violations = append(res.Violations, err.Error())
	}
	raw, err := domain.InvertScale(engValue, p.Scale, p.Offset)
	if err != nil {
		res.OK = false
		res.Violations = append(res.Violations, err.Error())
	} else {
		res.Raw = raw
	}

	// bool_bit: read current register first, same as the real write path
	// (read-only bus access; the write itself is not performed).
	var existing []uint16
	if p.Type == domain.TypeBoolBit {
		regs, err := s.modbus.ReadHoldingRegisters(d.Endpoint, d.UnitID, d.TimeoutMs, p.Address, 1)
		if err != nil {
			s.recordErr(err)
			return nil, fmt.Errorf("preview read %s: %w", name, err)
		}
		existing = regs
	}

	regs, err := domain.EncodeRegisters(*p, res.Raw, existing)
	if err != nil {
		res.OK = false
		res.Violations = append(res.Violations, err.Error())
	} else {
		res.Registers = regs
		if len(regs) == 1 {
			res.WriteFunction = "single"
		} else {
			res.WriteFunction = "multiple"
		}
	}

	if p.Type == domain.TypeBoolBit && len(existing) > 0 && len(res.Registers) > 0 {
		bit := 0
		if p.Bit != nil {
			bit = *p.Bit
		}
		mask := uint16(1) << uint(bit)
		res.Mask = &MaskInfo{
			Address:   p.Address,
			Bit:       bit,
			Mask:      mask,
			Existing:  existing[0],
			Preserved: existing[0] &^ mask,
			Merged:    res.Registers[0],
		}
	}

	digest := previewDigest(deviceID, name, engValue)
	token, exp := s.previews.issue(username, digest, s.previewTTL)
	res.PreviewToken = token
	res.ExpiresAt = exp.UTC().Format(time.RFC3339)
	res.TTLSeconds = int(s.previewTTL.Seconds())
	return res, nil
}
