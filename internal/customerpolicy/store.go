package customerpolicy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type store struct {
	dir string
}

func newStore(configFilePath, configuredPath string) (*store, error) {
	dir, err := resolveStoreDir(configFilePath, configuredPath)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, defaultStateDirectoryMode); err != nil {
		return nil, fmt.Errorf("create customer policy store: %w", err)
	}
	return &store{dir: dir}, nil
}

func resolveStoreDir(configFilePath, configuredPath string) (string, error) {
	path := strings.TrimSpace(configuredPath)
	if path == "" {
		base := strings.TrimSpace(configFilePath)
		if base != "" {
			path = filepath.Join(filepath.Dir(base), ".customer-key-policy")
		} else {
			wd, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("resolve working directory: %w", err)
			}
			path = filepath.Join(wd, ".customer-key-policy")
		}
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		rest := strings.TrimLeft(strings.TrimPrefix(path, "~"), `/\`)
		path = filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(rest, "\\", "/")))
	}
	if !filepath.IsAbs(path) {
		base := strings.TrimSpace(configFilePath)
		if base != "" {
			path = filepath.Join(filepath.Dir(base), path)
		}
	}
	return filepath.Clean(path), nil
}

func (s *store) policiesPath() string { return filepath.Join(s.dir, "customer-key-policies.json") }
func (s *store) usagePath() string    { return filepath.Join(s.dir, "customer-key-usage.json") }
func (s *store) recordsPath() string  { return filepath.Join(s.dir, "customer-key-records.jsonl") }
func (s *store) pricesPath() string   { return filepath.Join(s.dir, "model-prices-litellm.json") }

func (s *store) loadPolicies() (PolicyFile, error) {
	var file PolicyFile
	if err := readJSONFile(s.policiesPath(), &file); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PolicyFile{Version: 1, Policies: nil}, nil
		}
		return PolicyFile{}, err
	}
	if file.Version == 0 {
		file.Version = 1
	}
	return file, nil
}

func (s *store) savePolicies(file PolicyFile) error {
	file.Version = 1
	return writeJSONFileAtomic(s.policiesPath(), file)
}

func (s *store) loadUsage() (UsageFile, error) {
	var file UsageFile
	if err := readJSONFile(s.usagePath(), &file); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return UsageFile{Version: 1, Counters: make(map[string]*CounterState)}, nil
		}
		return UsageFile{}, err
	}
	if file.Version == 0 {
		file.Version = 1
	}
	if file.Counters == nil {
		file.Counters = make(map[string]*CounterState)
	}
	return file, nil
}

func (s *store) saveUsage(file UsageFile) error {
	file.Version = 1
	if file.Counters == nil {
		file.Counters = make(map[string]*CounterState)
	}
	return writeJSONFileAtomic(s.usagePath(), file)
}

func (s *store) loadPrices() (PriceCatalog, error) {
	var catalog PriceCatalog
	if err := readJSONFile(s.pricesPath(), &catalog); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PriceCatalog{Version: 1, Prices: make(map[string]ModelPrice)}, nil
		}
		return PriceCatalog{}, err
	}
	if catalog.Version == 0 {
		catalog.Version = 1
	}
	if catalog.Prices == nil {
		catalog.Prices = make(map[string]ModelPrice)
	}
	return catalog, nil
}

func (s *store) savePrices(catalog PriceCatalog) error {
	catalog.Version = 1
	if catalog.Prices == nil {
		catalog.Prices = make(map[string]ModelPrice)
	}
	return writeJSONFileAtomic(s.pricesPath(), catalog)
}

func (s *store) appendRecord(record AccessRecord, maxRecords int) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode access record: %w", err)
	}
	f, err := os.OpenFile(s.recordsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, defaultStateFileMode)
	if err != nil {
		return fmt.Errorf("open access records: %w", err)
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write access record: %w", err)
	}
	if errClose := f.Close(); errClose != nil {
		return fmt.Errorf("close access records: %w", errClose)
	}
	if maxRecords > 0 {
		return s.trimRecords(maxRecords)
	}
	return nil
}

func (s *store) trimRecords(maxRecords int) error {
	records, err := readRecordLines(s.recordsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(records) <= maxRecords {
		return nil
	}
	records = records[len(records)-maxRecords:]
	var buf bytes.Buffer
	for _, line := range records {
		if strings.TrimSpace(line) == "" {
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	return writeBytesAtomic(s.recordsPath(), buf.Bytes())
}

func (s *store) listRecords(filter RecordsFilter) ([]AccessRecord, error) {
	lines, err := readRecordLines(s.recordsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > defaultMaxManagementRecord {
		limit = defaultMaxManagementRecord
	}
	keyID := strings.TrimSpace(filter.KeyID)
	model := normalizeModel(filter.Model)
	status := strings.ToLower(strings.TrimSpace(filter.Status))
	out := make([]AccessRecord, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var record AccessRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if keyID != "" && record.KeyID != keyID {
			continue
		}
		if model != "" && normalizeModel(record.Model) != model && normalizeModel(record.Alias) != model {
			continue
		}
		if status != "" && strings.ToLower(record.Status) != status {
			continue
		}
		out = append(out, record)
	}
	return out, nil
}

func readJSONFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return os.ErrNotExist
	}
	if err = json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func writeJSONFileAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	data = append(data, '\n')
	return writeBytesAtomic(path, data)
}

func writeBytesAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, defaultStateDirectoryMode); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err = tmp.Chmod(defaultStateFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp state file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

func readRecordLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("read access records: %w", err)
	}
	return lines, nil
}
