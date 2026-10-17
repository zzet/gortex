package contracts

import (
	"regexp"
	"strings"
)

// NestJS RouterModule cross-module prefixing. A NestJS app can mount whole
// modules under a path with `RouterModule.register([{ path, module, children }])`,
// so a controller's effective route prefix is the path its module is mounted at
// (walked through nested `children`) joined with the controller's own
// `@Controller('...')` prefix. Because the router config, the `@Module` that
// lists the controllers, and the controller itself routinely live in three
// different files, resolving this prefix is a cross-file graph walk — the
// multi-file reach a same-file regex scanner cannot do.

var (
	nestControllerClassRE = regexp.MustCompile(`(?:export\s+)?class\s+([A-Za-z_]\w*)`)
	nestObjStringRE       = func(key string) *regexp.Regexp {
		return regexp.MustCompile(key + `\s*:\s*['"` + "`" + `]([^'"` + "`" + `]+)['"` + "`" + `]`)
	}
	nestObjIdentRE = func(key string) *regexp.Regexp {
		return regexp.MustCompile(key + `\s*:\s*([A-Za-z_]\w*)`)
	}
	nestControllersListRE = regexp.MustCompile(`controllers\s*:\s*\[([^\]]*)\]`)
)

func nestFileModuleFacts(text string) (map[string]string, map[string]string) {
	modulePrefix := map[string]string{}
	controllerModule := map[string]string{}
	if strings.Contains(text, "RouterModule.") {
		for _, call := range []string{"RouterModule.register(", "RouterModule.forRoot(", "RouterModule.forChild("} {
			for _, idx := range indexAll(text, call) {
				if arr, end := balancedSlice(text, idx, '[', ']'); end >= 0 {
					walkNestRouterRoutes(arr, "", modulePrefix)
				}
			}
		}
	}
	if strings.Contains(text, "@Module(") {
		for _, mod := range extractNestModules(text) {
			for _, ctrl := range mod.controllers {
				if _, ok := controllerModule[ctrl]; !ok {
					controllerModule[ctrl] = mod.name
				}
			}
		}
	}
	return detachRouterMap(modulePrefix), detachRouterMap(controllerModule)
}

func joinNestModuleFacts(paths []string, facts map[string]RouterFacts) map[string]string {
	modulePrefix := map[string]string{}
	controllerModule := map[string]string{}
	for _, path := range paths {
		for mod, prefix := range facts[path].ModulePrefix {
			modulePrefix[mod] = prefix
		}
		for ctrl, mod := range facts[path].ControllerModule {
			if _, ok := controllerModule[ctrl]; !ok {
				controllerModule[ctrl] = mod
			}
		}
	}

	out := map[string]string{}
	for ctrl, mod := range controllerModule {
		if p, ok := modulePrefix[mod]; ok && p != "" {
			out[ctrl] = p
		}
	}
	return out
}

// walkNestRouterRoutes walks a RouterModule routes array, recording each
// module's full mounted prefix (parent paths joined through `children`).
func walkNestRouterRoutes(arr, parent string, out map[string]string) {
	for _, obj := range splitTopLevelObjects(arr) {
		head := obj
		childrenArr := ""
		if ci := strings.Index(obj, "children"); ci >= 0 {
			head = obj[:ci]
			if a, end := balancedSlice(obj[ci:], 0, '[', ']'); end >= 0 {
				childrenArr = a
			}
		}
		pathSeg := firstSubmatch(nestObjStringRE("path"), head)
		module := firstSubmatch(nestObjIdentRE("module"), head)
		prefix := parent
		if pathSeg != "" {
			prefix = joinPaths(parent, "/"+strings.Trim(pathSeg, "/"))
		}
		if module != "" {
			out[module] = prefix
		}
		if childrenArr != "" {
			walkNestRouterRoutes(childrenArr, prefix, out)
		}
	}
}

// nestModule is a parsed @Module: its class name and the controllers it lists.
type nestModule struct {
	name        string
	controllers []string
}

// extractNestModules parses every @Module({ controllers: [...] }) and the class
// declared immediately after it.
func extractNestModules(text string) []nestModule {
	var out []nestModule
	for _, idx := range indexAll(text, "@Module(") {
		args, end := balancedSlice(text, idx, '(', ')')
		if end < 0 {
			continue
		}
		name := firstSubmatch(nestControllerClassRE, text[end:])
		if name == "" {
			continue
		}
		var controllers []string
		if cm := nestControllersListRE.FindStringSubmatch(args); cm != nil {
			for _, id := range strings.Split(cm[1], ",") {
				if id = strings.TrimSpace(id); id != "" {
					controllers = append(controllers, id)
				}
			}
		}
		out = append(out, nestModule{name: name, controllers: controllers})
	}
	return out
}

// --- small parsing helpers ----------------------------------------------

// balancedSlice finds the first `open` byte at or after fromIdx and returns the
// content up to its matching `close`, plus the index of that close (or -1).
// String/template literals are skipped so a brace inside a quote is ignored.
func balancedSlice(text string, fromIdx int, open, close byte) (string, int) {
	i := fromIdx
	for i < len(text) && text[i] != open {
		i++
	}
	if i >= len(text) {
		return "", -1
	}
	start := i
	depth := 0
	for i < len(text) {
		switch text[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return text[start+1 : i], i
			}
		case '\'', '"', '`':
			q := text[i]
			i++
			for i < len(text) && text[i] != q {
				if text[i] == '\\' {
					i++
				}
				i++
			}
		}
		i++
	}
	return "", -1
}

// splitTopLevelObjects returns each `{...}` object at the top level of an array
// body, depth-aware so nested objects stay inside their parent.
func splitTopLevelObjects(arr string) []string {
	var out []string
	for i := 0; i < len(arr); {
		if arr[i] == '{' {
			if inner, end := balancedSlice(arr, i, '{', '}'); end >= 0 {
				out = append(out, inner)
				i = end + 1
				continue
			}
		}
		i++
	}
	return out
}

func indexAll(text, sub string) []int {
	var out []int
	for i := 0; ; {
		j := strings.Index(text[i:], sub)
		if j < 0 {
			break
		}
		out = append(out, i+j)
		i += j + len(sub)
	}
	return out
}

func firstSubmatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}
