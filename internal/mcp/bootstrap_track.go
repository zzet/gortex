package mcp

import (
	"encoding/json"
	"strings"
)

// BootstrapTrackPath reads only the path a bootstrap mutation would use. The
// facade path goes through its real lowering rules, including target.file and
// envelope precedence, so the admission guard cannot inspect another path
// than the handler. Malformed calls remain the tool validator's responsibility.
func BootstrapTrackPath(frame []byte) (string, bool) {
	var call struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &call) != nil || call.Method != "tools/call" {
		return "", false
	}
	args := call.Params.Arguments
	switch call.Params.Name {
	case "workspace_admin":
		operation, _ := args["operation"].(string)
		if !strings.EqualFold(strings.TrimSpace(operation), "track") {
			return "", false
		}
		args = normalizeFacadeArguments(facadeOperationSpec{Facade: "workspace_admin", Operation: "track", Legacy: "track_repository"}, args)
	case "track_repository", "index_repository":
	default:
		return "", false
	}
	path, ok := args["path"].(string)
	return path, ok && strings.TrimSpace(path) != ""
}
