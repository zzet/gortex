package platform

import (
	"errors"
	"os"
	"strings"
)

func processBootScope() (string, error) {
	content, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return normalizeProcessBootScope(content)
}

func normalizeProcessBootScope(content []byte) (string, error) {
	scope := strings.TrimSpace(string(content))
	if len(scope) != 36 {
		return "", errors.New("native Linux boot_id has invalid length")
	}
	for i, ch := range scope {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if ch == '-' {
				continue
			}
		} else if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F' {
			continue
		}
		return "", errors.New("native Linux boot_id has invalid UUID encoding")
	}
	return strings.ToLower(scope), nil
}
