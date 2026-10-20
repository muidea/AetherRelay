package modeltrace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const Version = "0.2.0"
const MaxOutputs = 1000
const MaxTextBytes = 1 << 20
const MaxTotalTextBytes = 8 << 20
const MaxBankBytes = 32 << 20

var (
	ErrAssetInvalid     = errors.New("ASSET_INVALID")
	ErrInvalidRequest   = errors.New("MODELTRACE_INVALID_REQUEST")
	ErrNoUsableOutputs  = errors.New("MODELTRACE_NO_USABLE_OUTPUTS")
	ErrUninitialized    = errors.New("MODELTRACE_UNINITIALIZED")
	ErrNumericalFailure = errors.New("MODELTRACE_NUMERICAL_FAILURE")
)

// ParserPolicy must match the protocol that produced the model outputs.
type ParserPolicy string

const (
	GuardStrict    ParserPolicy = "guard_strict"
	CoreCompatible ParserPolicy = "modeltrace_core"
)

// Request contains values only. The caller owns sampling and evidence validation.
// CoreCompatible applies the upstream core's run extraction and rejection cutoff.
// An empty Parser selects GuardStrict, which rejects any malformed sample.
type Request struct {
	Outputs        []Output     `json:"outputs"`
	Parser         ParserPolicy `json:"parser_policy"`
	RequestedModel string       `json:"requested_model,omitempty"`
}

// Snapshot identifies the immutable bank used for a result, including custom banks.
type Snapshot struct {
	BankSHA256  string `json:"bank_sha256"`
	Schema      string `json:"schema"`
	ModelCount  int    `json:"model_count"`
	PortVersion string `json:"port_version"`
}

// Result is a closed-set ranking, never an assertion of backend identity or quality.
type Result struct {
	Analysis             *Analysis `json:"analysis"`
	Snapshot             Snapshot  `json:"snapshot"`
	RequestedModel       string    `json:"requested_model,omitempty"`
	RequestedModelInBank bool      `json:"requested_model_in_bank"`
	ClosedSet            bool      `json:"closed_set"`
}

// Engine owns one validated immutable bank. It has no lifecycle or I/O and is
// safe for concurrent Score calls; returned results never share mutable state.
// Construct with New, or use assets.Load for the embedded reference bank.
type Engine struct {
	bank     *bank
	snapshot Snapshot
}

// New decodes caller-owned bank bytes into a private snapshot. Mutating those
// bytes after New returns does not change the Engine.
func New(bankJSON []byte) (*Engine, error) {
	if len(bankJSON) == 0 || len(bankJSON) > MaxBankBytes {
		return nil, fmt.Errorf("%w: bank size", ErrAssetInvalid)
	}
	b, err := loadBank(bankJSON)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bankJSON)
	return &Engine{bank: b, snapshot: Snapshot{BankSHA256: hex.EncodeToString(sum[:]), Schema: b.Schema, ModelCount: len(b.Models), PortVersion: Version}}, nil
}

func (e *Engine) Snapshot() Snapshot {
	if e == nil {
		return Snapshot{}
	}
	return e.snapshot
}
func (e *Engine) HasModel(id string) bool {
	if e == nil || e.bank == nil {
		return false
	}
	for _, m := range e.bank.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// Score performs bounded, synchronous work. Context cancellation is checked
// before validation and between output/model stages; it starts no goroutines.
// The caller must not mutate req.Outputs while this call is running.
func (e *Engine) Score(ctx context.Context, req Request) (*Result, error) {
	if e == nil || e.bank == nil {
		return nil, ErrUninitialized
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Outputs) == 0 || len(req.Outputs) > MaxOutputs {
		return nil, fmt.Errorf("%w: output count", ErrInvalidRequest)
	}
	policy := req.Parser
	if policy == "" {
		policy = GuardStrict
	}
	if policy != GuardStrict && policy != CoreCompatible {
		return nil, fmt.Errorf("%w: parser policy", ErrInvalidRequest)
	}
	totalText := 0
	for i, o := range req.Outputs {
		totalText += len(o.Text)
		if totalText > MaxTotalTextBytes {
			return nil, fmt.Errorf("%w: aggregate text size", ErrInvalidRequest)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(o.Text) > MaxTextBytes || o.ExpectedCount < 0 || o.ExpectedCount > 1000 {
			return nil, fmt.Errorf("%w: output %d bounds", ErrInvalidRequest, i)
		}
		if policy == GuardStrict {
			if o.ExpectedCount == 0 {
				return nil, fmt.Errorf("%w: output %d expected count", ErrInvalidRequest, i)
			}
			if _, err := ValidateNumbers(o.Text, o.ExpectedCount); err != nil {
				return nil, fmt.Errorf("output %d: %w", i, err)
			}
		}
	}
	a, err := analyze(ctx, req.Outputs, e.bank)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, r := range a.Results {
		if !finite(r.Score) || !finite(r.Probability) || !finite(r.Similarity) || !finite(r.Conditional) {
			return nil, ErrNumericalFailure
		}
	}
	return &Result{Analysis: a, Snapshot: e.snapshot, RequestedModel: req.RequestedModel, RequestedModelInBank: req.RequestedModel != "" && e.HasModel(req.RequestedModel), ClosedSet: true}, nil
}
