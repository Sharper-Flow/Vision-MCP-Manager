package admin

import (
	"regexp"
	"strconv"
	"strings"
)

var codeModeIdentifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func deriveNamespacedName(toolName string, serverNames []string) string {
	server := ""
	for _, candidate := range serverNames {
		if strings.HasPrefix(toolName, candidate+"_") && len(candidate) > len(server) {
			server = candidate
		}
	}
	if server == "" {
		return ""
	}

	tool := strings.TrimPrefix(toolName, server+"_")
	return codeModeProperty(codeModeProperty("tools", server), tool)
}

func codeModeProperty(base, property string) string {
	if codeModeIdentifier.MatchString(property) {
		return base + "." + property
	}
	return base + "[" + strconv.Quote(property) + "]"
}
