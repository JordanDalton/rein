package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Repair only absent files. Validate every target before writing anything and
// retain the original receipt so undo still restores the pre-installation state.
func repairPersistentMissing(host string, apply bool) ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(cwd, ".rein", "harnesses", host+".persistent.json")
	if err := rejectHarnessSymlinks(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt persistentReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, err
	}
	if err := validatePersistentReceipt(receipt, host); err != nil {
		return nil, err
	}
	var missing []string
	for _, f := range receipt.Files {
		current, err := readPersistentFile(filepath.Join(cwd, f.Path))
		if err != nil {
			return nil, err
		}
		if !current.Existed {
			missing = append(missing, f.Path)
		} else if !bytes.Equal(current.Before, f.After) {
			return nil, fmt.Errorf("configuration drift in %s; refusing to overwrite edits (repair only restores missing files)", f.Path)
		}
	}
	if !apply {
		return missing, nil
	}
	for _, name := range missing {
		for _, f := range receipt.Files {
			if f.Path != name {
				continue
			}
			target := filepath.Join(cwd, name)
			if err := rejectHarnessSymlinks(target); err != nil {
				return missing, err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return missing, err
			}
			if err := writeHarnessExclusive(target, f.After); err != nil {
				return missing, fmt.Errorf("repair stopped at %s; any already restored files remain recoverable with --persistent --undo: %w", name, err)
			}
		}
	}
	return missing, nil
}
