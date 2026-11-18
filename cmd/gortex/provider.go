package main

import (
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/registry"
)

// provider.go is the `gortex provider` command tree — a CLI front end
// for the custom OpenAI-compatible provider registry (providers.json).
// It lets users register arbitrary endpoints (OpenRouter, Groq,
// Together, a self-hosted gateway, …) by name, then select one via
// `llm.provider` / GORTEX_LLM_PROVIDER like any built-in provider.

var providerCmd = &cobra.Command{
	Use:   "provider",
	Short: "Manage custom OpenAI-compatible LLM providers",
	Long: "Manage the registry of custom OpenAI-compatible LLM providers " +
		"(providers.json). A registered provider can be selected with " +
		"`llm.provider: <name>` in config or GORTEX_LLM_PROVIDER=<name>, " +
		"exactly like a built-in provider.",
}

var providerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered custom providers",
	Args:  cobra.NoArgs,
	RunE:  runProviderList,
}

var providerShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show one custom provider's full definition",
	Args:  cobra.ExactArgs(1),
	RunE:  runProviderShow,
}

var providerRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove a custom provider",
	Args:    cobra.ExactArgs(1),
	RunE:    runProviderRemove,
}

var providerAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Register (or replace) a custom OpenAI-compatible provider",
	Long: "Register a custom OpenAI-compatible provider. --base-url and " +
		"--model are required; the base URL should include any version " +
		"segment (e.g. https://api.groq.com/openai/v1) — gortex appends " +
		"/chat/completions. --preset <gateway> starts from a built-in " +
		"gateway preset's values (see `gortex provider list`), and any " +
		"other flag overrides that field. A provider named like a preset " +
		"replaces the preset.",
	Args: cobra.ExactArgs(1),
	RunE: runProviderAdd,
}

// flags for `provider add`.
var (
	providerBaseURL     string
	providerModel       string
	providerAPIKeyEnv   string
	providerSchemaMode  string
	providerProfile     string
	providerMaxTokensF  string
	providerReasoning   string
	providerTemperature float64
	providerHasTemp     bool
	providerHeaders     []string
	providerPriceInput  float64
	providerPriceOutput float64
	providerPreset      string
)

func init() {
	providerAddCmd.Flags().StringVar(&providerBaseURL, "base-url", "", "OpenAI-compatible API base, incl. version segment (required)")
	providerAddCmd.Flags().StringVar(&providerModel, "model", "", "default model identifier (required)")
	providerAddCmd.Flags().StringVar(&providerAPIKeyEnv, "api-key-env", "", "env var holding the bearer key (omit for keyless local endpoints)")
	providerAddCmd.Flags().StringVar(&providerSchemaMode, "schema-mode", "", "structured-output mode: json_schema (default) | json_object | prompt")
	providerAddCmd.Flags().StringVar(&providerProfile, "prompt-profile", "", "search-assist prompt tier: small (default) | frontier — use frontier for hosted frontier models")
	providerAddCmd.Flags().StringVar(&providerMaxTokensF, "max-tokens-field", "", "override the output-token body key (default max_completion_tokens)")
	providerAddCmd.Flags().StringVar(&providerReasoning, "reasoning-effort", "", "value sent as reasoning_effort (e.g. low|medium|high)")
	providerAddCmd.Flags().Float64Var(&providerTemperature, "temperature", 0, "temperature to send (only sent when --temperature is set)")
	providerAddCmd.Flags().StringArrayVar(&providerHeaders, "header", nil, "extra request header as key=value (repeatable)")
	providerAddCmd.Flags().Float64Var(&providerPriceInput, "price-input", 0, "informational USD per 1M input tokens")
	providerAddCmd.Flags().Float64Var(&providerPriceOutput, "price-output", 0, "informational USD per 1M output tokens")
	providerAddCmd.Flags().StringVar(&providerPreset, "preset", "", "start from a gateway preset's values (e.g. requesty); other flags override")

	providerCmd.AddCommand(providerListCmd)
	providerCmd.AddCommand(providerShowCmd)
	providerCmd.AddCommand(providerAddCmd)
	providerCmd.AddCommand(providerRemoveCmd)
	rootCmd.AddCommand(providerCmd)
}

func runProviderAdd(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	providerHasTemp = cmd.Flags().Changed("temperature")

	headers, err := parseHeaderFlags(providerHeaders)
	if err != nil {
		return err
	}
	var cp llm.CustomProvider
	if p := strings.TrimSpace(providerPreset); p != "" {
		preset, ok := llm.Preset(p)
		if !ok {
			return fmt.Errorf("provider add %q: unknown preset %q (available: %s)", name, p, strings.Join(llm.PresetNames(), ", "))
		}
		cp = preset
	}
	// Each flag that was given overrides the (preset or empty) field.
	setIf := func(dst *string, v string) {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
	setIf(&cp.BaseURL, providerBaseURL)
	setIf(&cp.Model, providerModel)
	setIf(&cp.APIKeyEnv, providerAPIKeyEnv)
	setIf(&cp.SchemaMode, providerSchemaMode)
	setIf(&cp.PromptProfile, providerProfile)
	setIf(&cp.MaxTokensField, providerMaxTokensF)
	setIf(&cp.ReasoningEffort, providerReasoning)
	if len(headers) > 0 {
		if cp.Headers == nil {
			cp.Headers = make(map[string]string, len(headers))
		}
		maps.Copy(cp.Headers, headers)
	}
	if cmd.Flags().Changed("price-input") {
		cp.Pricing.Input = providerPriceInput
	}
	if cmd.Flags().Changed("price-output") {
		cp.Pricing.Output = providerPriceOutput
	}
	if providerHasTemp {
		t := providerTemperature
		cp.Temperature = &t
	}

	if err := registry.Add(name, cp); err != nil {
		return fmt.Errorf("provider add %q: %w", name, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Registered custom provider %q -> %s (model %s)\n", name, cp.BaseURL, cp.Model)
	fmt.Fprintf(cmd.OutOrStdout(), "Select it with: llm.provider: %s   (or GORTEX_LLM_PROVIDER=%s)\n", name, name)
	if cp.APIKeyEnv != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Set the key in $%s before use.\n", cp.APIKeyEnv)
	}
	return nil
}

func runProviderRemove(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	removed, err := registry.Remove(name)
	if err != nil {
		return fmt.Errorf("provider remove %q: %w", name, err)
	}
	if !removed {
		fmt.Fprintf(cmd.OutOrStdout(), "No custom provider named %q\n", name)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed custom provider %q\n", name)
	return nil
}

func runProviderList(cmd *cobra.Command, _ []string) error {
	entries, err := registry.List()
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	user := make(map[string]bool, len(entries))
	if len(entries) == 0 {
		fmt.Fprintf(w, "No custom providers registered.\nAdd one with: gortex provider add <name> --base-url <url> --model <model>\n")
	} else {
		printProviderHeader(w)
		for _, e := range entries {
			user[e.Name] = true
			printProviderRow(w, e.Name, e.Provider)
		}
	}

	fmt.Fprintf(w, "\nGateway presets (select with llm.provider: <name>; a custom provider of the same name replaces the preset):\n")
	printProviderHeader(w)
	for _, name := range llm.PresetNames() {
		cp, _ := llm.Preset(name)
		label := name
		if user[name] {
			label += " (overridden)"
		}
		printProviderRow(w, label, cp)
	}
	return nil
}

func printProviderHeader(w io.Writer) {
	fmt.Fprintf(w, "%-16s  %-40s  %-24s  %-12s  %s\n", "NAME", "BASE_URL", "MODEL", "SCHEMA", "PROFILE")
}

func printProviderRow(w io.Writer, name string, cp llm.CustomProvider) {
	mode := cp.SchemaMode
	if mode == "" {
		mode = "json_schema"
	}
	fmt.Fprintf(w, "%-16s  %-40s  %-24s  %-12s  %s\n", name, cp.BaseURL, cp.Model, mode, promptProfileLabel(cp.PromptProfile))
}

func runProviderShow(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	cp, ok, err := registry.Get(name)
	if err != nil {
		return err
	}
	source := "custom provider (providers.json)"
	if ok {
		if _, preset := llm.Preset(name); preset {
			source += ", replaces the gateway preset of the same name"
		}
	} else if cp, ok = llm.Preset(name); ok {
		source = "gateway preset (built in; `gortex provider add " + name + " --preset " + name + "` to customise)"
	} else {
		return fmt.Errorf("no custom provider or gateway preset named %q", name)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "name:             %s\n", name)
	fmt.Fprintf(w, "source:           %s\n", source)
	fmt.Fprintf(w, "base_url:         %s\n", cp.BaseURL)
	fmt.Fprintf(w, "model:            %s\n", cp.Model)
	if cp.APIKeyEnv != "" {
		fmt.Fprintf(w, "api_key_env:      %s\n", cp.APIKeyEnv)
	}
	mode := cp.SchemaMode
	if mode == "" {
		mode = "json_schema (default)"
	}
	fmt.Fprintf(w, "schema_mode:      %s\n", mode)
	profile := promptProfileLabel(cp.PromptProfile)
	if cp.PromptProfile == "" {
		profile += " (default)"
	}
	fmt.Fprintf(w, "prompt_profile:   %s\n", profile)
	if cp.MaxTokensField != "" {
		fmt.Fprintf(w, "max_tokens_field: %s\n", cp.MaxTokensField)
	}
	if cp.Temperature != nil {
		fmt.Fprintf(w, "temperature:      %g\n", *cp.Temperature)
	}
	if cp.ReasoningEffort != "" {
		fmt.Fprintf(w, "reasoning_effort: %s\n", cp.ReasoningEffort)
	}
	if len(cp.Headers) > 0 {
		keys := make([]string, 0, len(cp.Headers))
		for k := range cp.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "header:           %s=%s\n", k, cp.Headers[k])
		}
	}
	if cp.Pricing.Input != 0 || cp.Pricing.Output != 0 {
		fmt.Fprintf(w, "pricing:          $%.4f in / $%.4f out per 1M tokens\n", cp.Pricing.Input, cp.Pricing.Output)
	}
	return nil
}

// promptProfileLabel renders a prompt_profile value, naming the
// implicit "small" default.
func promptProfileLabel(p string) string {
	if p == "" {
		return "small"
	}
	return p
}

// parseHeaderFlags turns repeated --header key=value flags into a map.
func parseHeaderFlags(flags []string) (map[string]string, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(flags))
	for _, f := range flags {
		k, v, ok := strings.Cut(f, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --header %q (want key=value)", f)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}
