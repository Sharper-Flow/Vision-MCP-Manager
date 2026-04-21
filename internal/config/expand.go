package config

import "fmt"

func expandSlotGroups(cfg *Config) error {
	if cfg == nil || len(cfg.SlotGroups) == 0 {
		return nil
	}
	if cfg.Servers == nil {
		cfg.Servers = make(map[string]*ServerConfig)
	}

	for groupName, group := range cfg.SlotGroups {
		if group == nil {
			return fmt.Errorf("config: slot group %q is nil", groupName)
		}
		if group.Count < 2 {
			return fmt.Errorf("config: slot group %q count must be >= 2", groupName)
		}
		for i := 1; i <= group.Count; i++ {
			name := fmt.Sprintf("%s-%d", group.Template, i)
			if _, exists := cfg.Servers[name]; exists {
				return fmt.Errorf("config: slot group %q generated server %q that already exists", groupName, name)
			}

			var server ServerConfig
			if group.Defaults != nil {
				server = *group.Defaults
			}
			server.Port = group.BasePort + i - 1
			server.SlotGroup = groupName
			server.SlotIndex = i
			cfg.Servers[name] = &server
		}
	}

	return nil
}
