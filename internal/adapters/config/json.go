package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"replication-service/internal/core/domain"
)

// JSONProvider implements the ports.ConfigProvider interface for JSON files.
type JSONProvider struct {
	filePath string
}

// NewJSONProvider creates a new JSONProvider.
func NewJSONProvider(filePath string) *JSONProvider {
	return &JSONProvider{filePath: filePath}
}

// LoadConfig reads and parses the bucardo.json file.
func (p *JSONProvider) LoadConfig(_ context.Context) (*domain.BucardoConfig, error) {
	jsonFile, err := os.Open(p.filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", p.filePath, err)
	}
	defer jsonFile.Close()

	byteValue, err := io.ReadAll(jsonFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", p.filePath, err)
	}

	var config domain.BucardoConfig
	if err := json.Unmarshal(byteValue, &config); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", p.filePath, err)
	}

	return &config, nil
}

// SaveConfig writes the configuration to the bucardo.json file.
func (p *JSONProvider) SaveConfig(_ context.Context, config *domain.BucardoConfig) error {
	byteValue, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(p.filePath, byteValue, 0644); err != nil {
		return fmt.Errorf("failed to write to %s: %w", p.filePath, err)
	}
	return nil
}
