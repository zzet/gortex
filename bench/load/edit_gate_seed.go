package main

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"strings"
)

// Faults are reachable only through the explicit standalone gate option and a
// binary built with the seed tag. Ambient GORTEX_* values remain filtered out.
func editGateSeedEnvironment(o runOptions) ([]string, error) {
	if o.EditGateSeed == "" {
		return nil, nil
	}
	if err := validateRunOptions(o); err != nil {
		return nil, err
	}
	info, err := buildinfo.ReadFile(o.Binary)
	if err != nil {
		return nil, fmt.Errorf("read edit gate seed binary metadata: %w", err)
	}
	for _, setting := range info.Settings {
		if setting.Key != "-tags" {
			continue
		}
		for tag := range strings.FieldsFuncSeq(setting.Value, func(r rune) bool { return r == ',' || r == ' ' }) {
			if tag == "gortex_edit_gate_seed" {
				return []string{"GORTEX_EDIT_GATE_SEED=" + o.EditGateSeed}, nil
			}
		}
	}
	return nil, errors.New("edit gate fault requires binary built with gortex_edit_gate_seed tag")
}
