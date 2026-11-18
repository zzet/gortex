package hooks

import "strings"

// PowerShell read, search and file-find cmdlets. Copilot's `powershell` tool
// and the Pi extension both map onto the Bash classifier (#586), so a model
// on native Windows reaching for `Get-Content` instead of `cat` has to get
// the same verdict. PowerShell command and parameter names are
// case-insensitive; values (paths, patterns) keep their case.

// powerShellReadCmdlets are Get-Content and its default aliases.
var powerShellReadCmdlets = map[string]bool{"get-content": true, "gc": true, "type": true}

// powerShellSearchCmdlets are Select-String and its default alias.
var powerShellSearchCmdlets = map[string]bool{"select-string": true, "sls": true}

// powerShellListCmdlets are Get-ChildItem and its default aliases. `ls` and
// `dir` are only taken as PowerShell when the call carries -Recurse, which no
// POSIX ls/dir accepts.
var powerShellListCmdlets = map[string]bool{"get-childitem": true, "gci": true, "dir": true, "ls": true}

// Parameters whose value is the next token (or follows a `:`), per cmdlet.
// Anything not listed is treated as a switch. The path parameters are listed
// separately so their value can be read.
var (
	powerShellPathParams = map[string]bool{"-path": true, "-literalpath": true, "-pspath": true, "-lp": true}

	// powerShellCommonValueParams are the value-taking common parameters
	// every cmdlet accepts, with their aliases. -Verbose, -Debug, -WhatIf
	// and -Confirm are switches and need no entry.
	powerShellCommonValueParams = map[string]bool{
		"-erroraction": true, "-ea": true,
		"-warningaction": true, "-wa": true,
		"-informationaction": true, "-infa": true,
		"-progressaction": true, "-proga": true,
		"-errorvariable": true, "-ev": true,
		"-warningvariable": true, "-wv": true,
		"-informationvariable": true, "-iv": true,
		"-outvariable": true, "-ov": true,
		"-outbuffer": true, "-ob": true,
		"-pipelinevariable": true, "-pv": true,
	}

	powerShellGetContentValueParams = map[string]bool{
		"-totalcount": true, "-head": true, "-first": true, "-tail": true, "-last": true,
		"-readcount": true, "-encoding": true, "-delimiter": true, "-stream": true,
		"-filter": true, "-include": true, "-exclude": true, "-credential": true,
	}

	powerShellSelectStringValueParams = map[string]bool{
		"-context": true, "-encoding": true, "-include": true, "-exclude": true, "-culture": true,
	}
)

// powerShellParam splits a `-Name:value` token into its lowercased name and
// the attached value.
func powerShellParam(token string) (name, value string, attached bool) {
	name, value, attached = strings.Cut(token, ":")
	return strings.ToLower(name), value, attached
}

// classifyPowerShellRead classifies tokens whose (lowercased) command is a
// PowerShell read, search or find cmdlet. ok is false when the command is not
// one of them or its shape is not recognised, so the caller can try the
// POSIX reading of the same name.
func classifyPowerShellRead(cmd string, tokens []string, dir string, dirKnown bool) (BashClassification, bool) {
	switch {
	case powerShellReadCmdlets[cmd]:
		if path, ok := extractPowerShellReadFile(tokens); ok {
			return BashClassification{Action: BashActionReadSource, Path: path, Primary: tokens[0]}, true
		}
	case powerShellSearchCmdlets[cmd]:
		if pattern, paths, ok := extractSelectString(tokens); ok {
			c := BashClassification{Action: BashActionGrepLike, Pattern: pattern, Primary: tokens[0]}
			if dirKnown {
				c.Paths = powerShellSearchPaths(paths, dir)
			}
			return c, true
		}
	case powerShellListCmdlets[cmd]:
		if name, ok := extractChildItemFilter(tokens); ok {
			return BashClassification{Action: BashActionFindName, Pattern: name, Primary: tokens[0]}, true
		}
	}
	return BashClassification{}, false
}

// extractPowerShellReadFile returns the file a Get-Content call reads: the
// -Path/-LiteralPath value, or else the first positional. Value-taking
// parameters (-TotalCount, -Head, -Tail, common ones like -ErrorAction, ...)
// have their value skipped so it is never taken for the file. A comma list
// or a non-source path is not a source read.
func extractPowerShellReadFile(tokens []string) (string, bool) {
	path, havePath := "", false
	var positionals []string
	for i := 1; i < len(tokens); i++ {
		t := tokens[i]
		if !strings.HasPrefix(t, "-") {
			positionals = append(positionals, t)
			continue
		}
		name, value, attached := powerShellParam(t)
		takesValue := powerShellPathParams[name] || powerShellGetContentValueParams[name] || powerShellCommonValueParams[name]
		if !takesValue {
			continue
		}
		if !attached {
			if i+1 >= len(tokens) {
				return "", false
			}
			i++
			value = tokens[i]
		}
		if powerShellPathParams[name] && !havePath {
			path, havePath = value, true
		}
	}
	if !havePath && len(positionals) > 0 {
		path = positionals[0]
	}
	if path == "" || strings.Contains(path, ",") || !looksLikeSourceFile(path) {
		return "", false
	}
	return path, true
}

// extractSelectString returns the pattern and the searched paths of a
// Select-String call. Named parameters are bound first, the way PowerShell
// binds them: the pattern is the -Pattern value or else the first
// positional, and the paths are the -Path/-LiteralPath values plus the
// remaining positionals. ok is false when no pattern is given, and when
// -InputObject is used: that parameter set searches the supplied text, not
// files, so it is no source search.
func extractSelectString(tokens []string) (string, []string, bool) {
	pattern, havePattern := "", false
	var paths, positionals []string
	for i := 1; i < len(tokens); i++ {
		t := tokens[i]
		if !strings.HasPrefix(t, "-") {
			positionals = append(positionals, t)
			continue
		}
		name, value, attached := powerShellParam(t)
		takesValue := name == "-pattern" || name == "-inputobject" || powerShellPathParams[name] ||
			powerShellSelectStringValueParams[name] || powerShellCommonValueParams[name]
		if !takesValue {
			continue
		}
		if !attached {
			if i+1 >= len(tokens) {
				return "", nil, false
			}
			i++
			value = tokens[i]
		}
		switch {
		case name == "-inputobject":
			return "", nil, false
		case name == "-pattern":
			pattern, havePattern = value, true
		case powerShellPathParams[name]:
			paths = append(paths, value)
		}
	}
	if !havePattern && len(positionals) > 0 {
		pattern, havePattern = positionals[0], true
		positionals = positionals[1:]
	}
	paths = append(paths, positionals...)
	return pattern, paths, havePattern
}

// powerShellSearchPaths resolves Select-String path operands like
// grepSearchPaths does: nil when none is named or any is not a literal path.
// A comma list names several paths in one token.
func powerShellSearchPaths(operands []string, dir string) []string {
	if len(operands) == 0 {
		return nil
	}
	var out []string
	for _, operand := range operands {
		for _, part := range strings.Split(operand, ",") {
			p, ok := literalShellPath(part)
			if !ok {
				return nil
			}
			out = append(out, joinShellPath(dir, p))
		}
	}
	return out
}

// extractChildItemFilter recognises `Get-ChildItem -Recurse -Filter <name>`
// (or -Include) and returns the name with leading/trailing wildcards
// stripped, the way extractFindName does for `find -name`. Without -Recurse
// the call is a directory listing, not a find.
func extractChildItemFilter(tokens []string) (string, bool) {
	recurse := false
	filter, haveFilter := "", false
	for i := 1; i < len(tokens); i++ {
		name, value, attached := powerShellParam(tokens[i])
		switch name {
		case "-recurse", "-r":
			if !attached || strings.EqualFold(value, "$true") {
				recurse = true
			}
		case "-filter", "-include":
			if !attached {
				if i+1 >= len(tokens) {
					return "", false
				}
				i++
				value = tokens[i]
			}
			if !haveFilter {
				filter, haveFilter = value, true
			}
		case "-path", "-literalpath", "-pspath", "-lp", "-exclude", "-depth", "-attributes":
			if !attached {
				i++
			}
		default:
			if powerShellCommonValueParams[name] && !attached {
				i++
			}
		}
	}
	if !recurse || !haveFilter {
		return "", false
	}
	return strings.Trim(filter, "*?"), true
}
