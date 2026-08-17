package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestEveryServerConfigSettingIsClassified is the drift guard for this whole
// mechanism.
//
// The capability coverage test proves that each governed setting has a row for
// each transport. It says nothing about a setting that was never added to the
// table at all -- which is exactly how the five settings this change fixes came
// to be accepted by a transport that never read them.
//
// This test closes that gap from the other side: it walks ServerConfig by
// reflection and requires every yaml-serialized field to be either governed by
// the capability table or explicitly listed as ungoverned with a reason. A new
// field is unclassified by construction, so adding one fails the build until
// somebody decides which transports honor it.
func TestEveryServerConfigSettingIsClassified(t *testing.T) {
	governed := make(map[SettingKey]bool, len(GovernedSettingKeys))
	for _, setting := range GovernedSettingKeys {
		governed[setting] = true
	}

	serverConfig := reflect.TypeOf(ServerConfig{})
	for i := 0; i < serverConfig.NumField(); i++ {
		field := serverConfig.Field(i)

		name := yamlFieldName(field.Tag.Get("yaml"))
		if name == "" || name == "-" {
			// Internal metadata such as SlotGroup and SlotIndex is never
			// user-authored, so it cannot be silently ignored user intent.
			continue
		}

		setting := SettingKey(name)
		_, isGoverned := governed[setting]
		rationale, isUngoverned := ungovernedSettings[setting]

		switch {
		case isGoverned && isUngoverned:
			t.Errorf("setting %q is both governed and ungoverned; it must be exactly one", setting)
		case !isGoverned && !isUngoverned:
			t.Errorf(
				"ServerConfig field %s (yaml %q) is unclassified: add it to the capability table if a transport's support for it varies, or to ungovernedSettings with a reason if every transport treats it the same",
				field.Name, setting,
			)
		case isUngoverned && strings.TrimSpace(rationale) == "":
			t.Errorf("ungoverned setting %q has an empty rationale; state why it needs no capability row", setting)
		}
	}
}

// TestUngovernedSettingsAllExist stops the allowlist rotting. An entry naming a
// field that no longer exists is stale documentation that would otherwise sit
// there indefinitely, and worse, could mask a real field renamed underneath it.
func TestUngovernedSettingsAllExist(t *testing.T) {
	actual := make(map[SettingKey]bool)
	serverConfig := reflect.TypeOf(ServerConfig{})
	for i := 0; i < serverConfig.NumField(); i++ {
		if name := yamlFieldName(serverConfig.Field(i).Tag.Get("yaml")); name != "" && name != "-" {
			actual[SettingKey(name)] = true
		}
	}

	for setting := range ungovernedSettings {
		if !actual[setting] {
			t.Errorf("ungovernedSettings lists %q, which is not a yaml field on ServerConfig", setting)
		}
	}
	for _, setting := range GovernedSettingKeys {
		if !actual[setting] {
			t.Errorf("GovernedSettingKeys lists %q, which is not a yaml field on ServerConfig", setting)
		}
	}
}

func yamlFieldName(tag string) string {
	if tag == "" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}
