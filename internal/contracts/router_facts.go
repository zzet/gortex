package contracts

import (
	"regexp"
	"sort"
	"strings"
)

var routeReceiverPatterns = map[string]*regexp.Regexp{
	"fastapi/flask": pyRouteReceiverRE, "flask": flaskRouteReceiverRE, "express": jsRouteReceiverRE,
	"gin/echo/chi": ginRouteReceiverRE, "fiber": ginRouteReceiverRE, "axum": axumLetBindingRE, "vapor": vaporRouteReceiverRE,
}

// routerLineFacts records the same nearest-decorator and enclosing-block
// semantics as the source readers, in one forward pass over the file.
func routerLineFacts(text string) []RouterLineFacts {
	lines := strings.Split(text, "\n")
	var out []RouterLineFacts
	var previous RouterLineFacts
	var nest, spring, actix string
	var rails, laravel, vapor []string
	for idx, line := range lines {
		if m := nestControllerRE.FindStringSubmatch(line); m != nil {
			nest = cleanPrefix(m[1])
		}
		if m := actixFnDefRE.FindStringSubmatch(line); m != nil {
			actix = m[1]
		}
		receivers := map[string]string{}
		for fw, re := range routeReceiverPatterns {
			if m := re.FindStringSubmatch(line); m != nil {
				receivers[fw] = strings.Clone(m[1])
			}
		}
		if actix != "" {
			receivers["actix"] = strings.Clone(actix)
		}
		current := RouterLineFacts{
			Line:      idx + 1,
			Receivers: receivers, NestPrefix: strings.Clone(nest), SpringPrefix: strings.Clone(spring),
			RailsPrefix: joinPaths(rails...), LaravelPrefix: joinPaths(laravel...), VaporPrefix: joinPaths(vapor...),
		}
		if !sameRouterContext(previous, current) {
			out = append(out, current)
			previous = current
		}

		if m := springClassMappingRE.FindStringSubmatch(line); m != nil {
			spring = cleanPrefix(m[1])
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "end" || strings.HasPrefix(trimmed, "end ") {
			if len(rails) > 0 {
				rails = rails[:len(rails)-1]
			}
		} else if railsLineOpensBlock(trimmed) {
			prefix := ""
			if m := railsNamespaceRE.FindStringSubmatch(line); m != nil {
				prefix = m[1]
			}
			rails = append(rails, prefix)
		}
		lp, vp := "", ""
		if m := laravelPrefixRE.FindStringSubmatch(line); m != nil {
			lp = cleanPrefix(m[1])
		}
		if m := vaporClosureGroupRE.FindStringSubmatch(line); m != nil {
			vp = vaporGroupSegments(m[1])
		}
		for j := 0; j < len(line); j++ {
			switch line[j] {
			case '{':
				laravel = append(laravel, lp)
				vapor = append(vapor, vp)
			case '}':
				if len(laravel) > 0 {
					laravel = laravel[:len(laravel)-1]
					vapor = vapor[:len(vapor)-1]
				}
			}
		}
	}
	return out
}

// Regex submatches alias their input; clone the retained strings so a tiny
// mount fact cannot keep an entire source file alive in the actor cache.
func detachRouterMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[strings.Clone(key)] = strings.Clone(value)
	}
	return out
}

func sameRouterContext(a, b RouterLineFacts) bool {
	if a.NestPrefix != b.NestPrefix || a.SpringPrefix != b.SpringPrefix || a.RailsPrefix != b.RailsPrefix || a.LaravelPrefix != b.LaravelPrefix || a.VaporPrefix != b.VaporPrefix || len(a.Receivers) != len(b.Receivers) {
		return false
	}
	for fw, receiver := range a.Receivers {
		if b.Receivers[fw] != receiver {
			return false
		}
	}
	return true
}

func (f RouterFacts) lineFacts(line int) RouterLineFacts {
	if line < 1 || line > f.LineCount {
		return RouterLineFacts{}
	}
	idx := sort.Search(len(f.Lines), func(i int) bool { return f.Lines[i].Line > line }) - 1
	if idx < 0 {
		return RouterLineFacts{}
	}
	return f.Lines[idx]
}

// Clone detaches all nested storage for an independently owned snapshot.
func (f RouterFacts) Clone() RouterFacts {
	f.SelfPrefix = detachRouterMap(f.SelfPrefix)
	f.MountPrefix = detachRouterMap(f.MountPrefix)
	f.MountParent = detachRouterMap(f.MountParent)
	f.ModulePrefix = detachRouterMap(f.ModulePrefix)
	f.ControllerModule = detachRouterMap(f.ControllerModule)
	f.ControllerClass = strings.Clone(f.ControllerClass)
	var lines []RouterLineFacts
	if f.Lines != nil {
		lines = make([]RouterLineFacts, len(f.Lines))
	}
	for i, line := range f.Lines {
		line.Receivers = detachRouterMap(line.Receivers)
		line.NestPrefix = strings.Clone(line.NestPrefix)
		line.SpringPrefix = strings.Clone(line.SpringPrefix)
		line.RailsPrefix = strings.Clone(line.RailsPrefix)
		line.LaravelPrefix = strings.Clone(line.LaravelPrefix)
		line.VaporPrefix = strings.Clone(line.VaporPrefix)
		lines[i] = line
	}
	f.Lines = lines
	return f
}
