package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"
)

type policy struct {
	Version    int         `yaml:"schema_version"`
	Baseline   baseline    `yaml:"baseline"`
	Allowances []allowance `yaml:"allowances"`
	Budgets    []budget    `yaml:"budgets"`
}

type baseline struct {
	Release       string  `yaml:"release"`
	Revision      string  `yaml:"revision"`
	NoiseFraction float64 `yaml:"noise_fraction"`
}

type allowance struct {
	Name          string   `yaml:"name"`
	OwningRelease string   `yaml:"owning_release"`
	Kind          string   `yaml:"kind"` // additive or guard_limit, never a hard cap
	Value         float64  `yaml:"value"`
	Unit          string   `yaml:"unit"`
	Metrics       []string `yaml:"metrics"`
	Scenarios     []string `yaml:"scenarios"`
}

type budget struct {
	Metric        string   `yaml:"metric"`
	Scenario      string   `yaml:"scenario"`
	HostClass     string   `yaml:"host_class"`
	OwningRelease string   `yaml:"owning_release"`
	Mode          string   `yaml:"mode"`
	Target        *float64 `yaml:"target"`
	Unit          string   `yaml:"unit"`
	Allowance     []string `yaml:"allowance"`
	Lane          []string `yaml:"lane"`
	Compare       string   `yaml:"compare"`
	MinRepeats    int      `yaml:"min_repeats"`
}

var releasePattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var planPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

const shippedRevision = "defa9f8e7a670792b5b693744d88488f741a8160"
const hostedSmokeClass = "hosted_arm_smoke"

func knownHostClass(class string) bool {
	return slices.Contains([]string{"reference_linux", "reference_mac", "large_mac", hostedSmokeClass}, class)
}

func finiteNonnegative(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

func loadPolicy(data []byte) (policy, error) {
	var p policy
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&p); err != nil {
		return p, fmt.Errorf("budgets: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return p, errors.New("budgets must contain exactly one YAML document")
	}
	// A not-yet-assigned absolute target is explicitly null, never accidentally
	// omitted. No aliases or YAML merge-key defaults for rows.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return p, err
	}
	if err := requireBudgetFields(&doc); err != nil {
		return p, err
	}
	return p, p.validate()
}

func requireBudgetFields(doc *yaml.Node) error {
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("budgets must be a mapping")
	}
	root := doc.Content[0]
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != "budgets" {
			continue
		}
		rows := root.Content[i+1]
		if rows.Kind != yaml.SequenceNode {
			return errors.New("budgets must be a sequence")
		}
		for index, row := range rows.Content {
			if row.Kind != yaml.MappingNode {
				return fmt.Errorf("budget %d must be an explicit mapping", index+1)
			}
			keys := make(map[string]bool)
			for k := 0; k < len(row.Content); k += 2 {
				keys[row.Content[k].Value] = true
			}
			for _, key := range []string{"metric", "scenario", "host_class", "owning_release", "mode", "target", "unit", "allowance", "lane", "compare"} {
				if !keys[key] {
					return fmt.Errorf("budget %d lacks %s", index+1, key)
				}
			}
		}
		return nil
	}
	return errors.New("missing budgets")
}

func (p policy) validate() error {
	if p.Version != 1 || p.Baseline.Release != "v0.64.10" || p.Baseline.Revision != shippedRevision || p.Baseline.NoiseFraction != .15 {
		return errors.New("expected schema1, shipped v0.64.10 revision and 15% guard noise policy")
	}
	if len(p.Budgets) == 0 {
		return errors.New("no budget rows")
	}
	allowances := make(map[string]allowance)
	for _, a := range p.Allowances {
		if _, duplicate := allowances[a.Name]; a.Name == "" || duplicate {
			return fmt.Errorf("empty or duplicate allowance %q", a.Name)
		}
		if !releasePattern.MatchString(a.OwningRelease) || !finiteNonnegative(a.Value) || (a.Kind != "additive" && a.Kind != "guard_limit") || len(a.Metrics) == 0 || len(a.Scenarios) == 0 {
			return fmt.Errorf("invalid allowance %q", a.Name)
		}
		for _, name := range a.Metrics {
			m, ok := metricSpecs[name]
			if !ok || m.unit != a.Unit || (m.axis != "disk" && m.axis != "memory") {
				return fmt.Errorf("allowance %s has incompatible metric %s", a.Name, name)
			}
		}
		for _, name := range a.Scenarios {
			if _, ok := scenarioSpecs[name]; !ok {
				return fmt.Errorf("allowance %s has unknown scenario %s", a.Name, name)
			}
		}
		allowances[a.Name] = a
	}
	seen := make(map[string]bool)
	for i, b := range p.Budgets {
		if err := validateBudget(b, allowances); err != nil {
			return fmt.Errorf("budget %d: %w", i+1, err)
		}
		key := b.HostClass + "/" + b.Scenario + "/" + b.Metric
		if seen[key] {
			return fmt.Errorf("duplicate budget %s", key)
		}
		seen[key] = true
	}
	return nil
}

func validateBudget(b budget, allowances map[string]allowance) error {
	m, ok := metricSpecs[b.Metric]
	if !ok || m.unit != b.Unit {
		return fmt.Errorf("unknown metric or incorrect unit: %s (%s)", b.Metric, b.Unit)
	}
	if _, ok := scenarioSpecs[b.Scenario]; !ok {
		return fmt.Errorf("unknown scenario %s", b.Scenario)
	}
	validOwner := releasePattern.MatchString(b.OwningRelease) || b.Mode == "guard" && b.OwningRelease == "unassigned"
	if !knownHostClass(b.HostClass) || !validOwner {
		return errors.New("invalid host class or owning release")
	}
	if b.HostClass == hostedSmokeClass && (b.Mode != "block" || len(b.Lane) != 1 || b.Lane[0] != "smoke") {
		return errors.New("hosted ARM budgets are block-only PR smoke targets, never nightly/release capacity")
	}
	if !slices.Contains([]string{"le", "lt", "ge", "eq"}, b.Compare) || len(b.Lane) == 0 {
		return errors.New("explicit comparison and lanes required")
	}
	if b.MinRepeats != 0 && b.MinRepeats < 3 {
		return errors.New("min_repeats cannot weaken the three-repeat minimum")
	}
	for i, lane := range b.Lane {
		if !slices.Contains([]string{"smoke", "nightly", "release"}, lane) || slices.Contains(b.Lane[:i], lane) {
			return fmt.Errorf("unknown or repeated lane %s", lane)
		}
	}
	switch b.Mode {
	case "block":
		if b.Target == nil || !finiteNonnegative(*b.Target) {
			return errors.New("block needs an absolute nonnegative target")
		}
	case "guard":
		if (b.Target != nil && !finiteNonnegative(*b.Target)) || b.Compare != "le" {
			return errors.New("guard needs a finite future target or explicit null, and comparison le")
		}
	default:
		return errors.New("mode must be block or guard")
	}
	if b.Unit == "fraction" && b.Target != nil && *b.Target > 1 {
		return errors.New("fraction target must be in [0,1], not percent")
	}
	for i, name := range b.Allowance {
		a, ok := allowances[name]
		if !ok || !slices.Contains(a.Metrics, b.Metric) || !slices.Contains(a.Scenarios, b.Scenario) || slices.Contains(b.Allowance[:i], name) {
			return fmt.Errorf("unknown, repeated or inapplicable allowance %s", name)
		}
	}
	return nil
}
