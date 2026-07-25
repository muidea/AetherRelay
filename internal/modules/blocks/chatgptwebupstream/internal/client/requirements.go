package client

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ai-proxy/internal/pkg/chatgptwebantibot"
	"github.com/google/uuid"
)

const defaultPoWScript = "https://chatgpt.com/backend-api/sentinel/sdk.js"

type Requirements struct {
	Token          string
	ProofToken     string
	TurnstileToken string
	SOToken        string
}

// ChatRequirements executes the authenticated sentinel prepare/finalize flow.
// Arkose challenges remain an explicit unsupported result rather than silently
// sending an incomplete finalize request.
func (c *Client) ChatRequirements() (Requirements, error) {
	if err := c.Bootstrap(); err != nil {
		return Requirements{}, err
	}
	config := requirementsConfig(c.userAgent, c.scriptSources, c.dataBuild)
	legacy, err := antibot.BuildLegacyRequirementsToken(config)
	if err != nil {
		return Requirements{}, err
	}
	prepareBody, err := c.postJSON("/backend-api/sentinel/chat-requirements/prepare", mustJSON(struct {
		P string `json:"p"`
	}{legacy}), "requirements_prepare")
	if err != nil {
		return Requirements{}, err
	}
	var prepare struct {
		PrepareToken string `json:"prepare_token"`
		Arkose       struct {
			Required bool `json:"required"`
		} `json:"arkose"`
		Proof struct {
			Required   bool   `json:"required"`
			Seed       string `json:"seed"`
			Difficulty string `json:"difficulty"`
		} `json:"proofofwork"`
		Turnstile struct {
			Required bool   `json:"required"`
			DX       string `json:"dx"`
		} `json:"turnstile"`
	}
	if err := json.Unmarshal(prepareBody, &prepare); err != nil {
		return Requirements{}, fmt.Errorf("decode requirements prepare: %w", err)
	}
	if prepare.Arkose.Required {
		return Requirements{}, fmt.Errorf("chat requirements requires arkose token")
	}
	proof := ""
	if prepare.Proof.Required {
		value, err := antibot.BuildProofToken(antibot.RequirementsSeed{Seed: prepare.Proof.Seed, Difficulty: prepare.Proof.Difficulty, Config: config})
		if err != nil {
			return Requirements{}, err
		}
		proof = string(value)
	}
	turnstile := ""
	if prepare.Turnstile.Required && prepare.Turnstile.DX != "" {
		// Match the Python client: Turnstile bytecode is opportunistic. The
		// server-side finalize endpoint can accept an empty value or provide a
		// different requirements response, while rejecting locally here prevents
		// that recovery path whenever a browser-only branch is encountered.
		if value, solveErr := antibot.SolveTurnstileToken(prepare.Turnstile.DX, legacy); solveErr == nil {
			turnstile = value
		}
	}
	finalizePayload := struct {
		PrepareToken   string `json:"prepare_token"`
		ProofToken     string `json:"proof_token"`
		TurnstileToken string `json:"turnstile_token"`
	}{prepare.PrepareToken, proof, turnstile}
	finalizeBody, err := c.postJSON("/backend-api/sentinel/chat-requirements/finalize", mustJSON(finalizePayload), "requirements_finalize")
	if err != nil {
		return Requirements{}, err
	}
	var finalize struct {
		Token   string `json:"token"`
		SOToken string `json:"so_token"`
	}
	if err := json.Unmarshal(finalizeBody, &finalize); err != nil {
		return Requirements{}, fmt.Errorf("decode requirements finalize: %w", err)
	}
	if finalize.Token == "" {
		return Requirements{}, fmt.Errorf("missing authenticated chat requirements token")
	}
	return Requirements{Token: finalize.Token, ProofToken: proof, TurnstileToken: turnstile, SOToken: finalize.SOToken}, nil
}

func requirementsConfig(userAgent string, sources []string, dataBuild string) []any {
	source := defaultPoWScript
	if len(sources) > 0 {
		source = sources[0]
	}
	now := time.Now()
	return []any{4480, now.UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)", 4294705152, 1, userAgent, source, dataBuild, "en-US", "en-US,es-US,en,es", 0.5, "webdriver−false", "location", "window", float64(now.UnixMilli()), uuid.NewString(), "", 16, float64(now.UnixMilli()), 0, 0, 0, 0, 0, 0, 0, 0}
}

var scriptSrcPattern = regexp.MustCompile(`(?i)<script[^>]+src=["']([^"']+)`)
var buildPattern = regexp.MustCompile(`c/[^/]*/_`)
var dataBuildPattern = regexp.MustCompile(`(?i)<html[^>]*data-build=["']([^"']*)`)

func parsePoWResources(html string) ([]string, string) {
	matches := scriptSrcPattern.FindAllStringSubmatch(html, -1)
	sources := make([]string, 0, len(matches))
	build := ""
	for _, match := range matches {
		sources = append(sources, match[1])
		if build == "" {
			build = buildPattern.FindString(match[1])
		}
	}
	if build == "" {
		if match := dataBuildPattern.FindStringSubmatch(html); len(match) == 2 {
			build = strings.TrimSpace(match[1])
		}
	}
	if len(sources) == 0 {
		sources = []string{defaultPoWScript}
	}
	return sources, build
}

func mustJSON(value any) string { data, _ := json.Marshal(value); return string(data) }
