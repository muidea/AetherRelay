package admin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ai-proxy/internal/pkg/aiproxyconfig"

	"go.yaml.in/yaml/v4"
)

// configRewrite keeps a fully validated replacement beside the active config
// until its runtime activation succeeds. This prevents a rejected hot reload
// from silently leaving a new, inactive configuration on disk.
type configRewrite struct {
	path     string
	tempPath string
	config   config.Config
}

func prepareConfigRewrite(path, expectedAdminBasePath string, mutate func(*yaml.Node) error) (*configRewrite, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("no writable config file is active")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	root, err := documentRoot(&document)
	if err != nil {
		return nil, err
	}
	if err := mutate(root); err != nil {
		return nil, err
	}

	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close config encoder: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".ai-proxy-config-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("create temporary config: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func(err error) (*configRewrite, error) {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return nil, err
	}
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		return cleanup(fmt.Errorf("set temporary config mode: %w", err))
	}
	if _, err := temp.Write(encoded.Bytes()); err != nil {
		return cleanup(fmt.Errorf("write temporary config: %w", err))
	}
	if err := temp.Close(); err != nil {
		return cleanup(fmt.Errorf("close temporary config: %w", err))
	}
	cfg, err := config.Load(tempPath)
	if err != nil {
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("configuration rejected: %w", err)
	}
	if cfg.AdminAuth.BasePath != expectedAdminBasePath {
		_ = os.Remove(tempPath)
		return nil, errAdminBasePathRestart
	}
	return &configRewrite{path: path, tempPath: tempPath, config: cfg}, nil
}

func (r *configRewrite) discard() {
	if r != nil && r.tempPath != "" {
		_ = os.Remove(r.tempPath)
	}
}

func (r *configRewrite) commit() error {
	if r == nil || r.tempPath == "" {
		return errors.New("configuration rewrite is unavailable")
	}
	if err := os.Rename(r.tempPath, r.path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	r.tempPath = ""
	return nil
}

// activateAndCommitConfig makes runtime and durable configuration change as a
// single Admin operation. If disk replacement fails after activation, it
// restores the previous runtime snapshot before reporting the failure.
func (h *Handler) activateAndCommitConfig(rewrite *configRewrite) error {
	if rewrite == nil {
		return errors.New("configuration rewrite is unavailable")
	}
	defer rewrite.discard()
	previous := h.runtime.ConfigSnapshot()
	if err := h.activateConfig(rewrite.config); err != nil {
		return err
	}
	if err := rewrite.commit(); err != nil {
		if rollbackErr := h.activateConfig(previous); rollbackErr != nil {
			return fmt.Errorf("replace config: %w; restore runtime configuration: %v", err, rollbackErr)
		}
		return err
	}
	return nil
}
