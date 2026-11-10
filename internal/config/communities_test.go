package config

import (
	"strings"
	"testing"
)

func TestCommunityDefinitionsConfig(t *testing.T) {
	// Omitted: disabled, nothing to read.
	c, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.CommunityDefinitions.Enabled || c.CommunityDefinitions.Directory != "" {
		t.Fatalf("not disabled by default: %+v", c.CommunityDefinitions)
	}
	// Enabled with a directory.
	c, err = Decode(strings.NewReader(validConfig + "community_definitions:\n  enabled: true\n  directory: /etc/hextet-lg/communities\n"))
	if err != nil || !c.CommunityDefinitions.Enabled || c.CommunityDefinitions.Directory != "/etc/hextet-lg/communities" {
		t.Fatalf("%+v %v", c.CommunityDefinitions, err)
	}
	// Disabled settings are accepted without a directory, and a directory alone is inert.
	for _, y := range []string{"community_definitions:\n  enabled: false\n", "community_definitions:\n  enabled: false\n  directory: /nowhere\n", "community_definitions: {}\n"} {
		if c, err = Decode(strings.NewReader(validConfig + y)); err != nil || c.CommunityDefinitions.Enabled {
			t.Errorf("%q: %v", y, err)
		}
	}
	// Enabled requires a directory; unknown keys are rejected.
	for _, y := range []string{"community_definitions:\n  enabled: true\n", "community_definitions:\n  enabled: true\n  directory: \"  \"\n", "community_definitions:\n  enabled: true\n  directory: /x\n  url: http://example.invalid/\n", "community_definitions:\n  enabled: maybe\n"} {
		if _, err := Decode(strings.NewReader(validConfig + y)); err == nil {
			t.Errorf("accepted %q", y)
		} else if strings.Contains(y, "enabled: true\n") && !strings.Contains(y, "url") && !strings.Contains(err.Error(), "community_definitions.directory") {
			t.Errorf("%q: unhelpful error %v", y, err)
		}
	}
}
