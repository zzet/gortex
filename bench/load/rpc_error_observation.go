package main

// Error data is decoded JSON. Keep a private snapshot when an observation
// outlives its RPC, including nested objects and lists of pending paths.
func copyRPCErrorData(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	out := make(map[string]any, len(data))
	for key, value := range data {
		out[key] = copyRPCErrorValue(value)
	}
	return out
}

func copyRPCErrorValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return copyRPCErrorData(value)
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = copyRPCErrorValue(item)
		}
		return out
	default:
		return value
	}
}
