package config

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"ORIGINCLOUD_PROJECT": "my-project"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Project != "projects/my-project" || c.WebhookAddress != "localhost:8888" || c.HealthAddress != ":8080" ||
		c.LogLevel != slog.LevelInfo || c.LogFormat != "text" || c.DryRun || c.Visibility != VisibilityAny {
		t.Errorf("defaults: %+v", c)
	}
}

func TestEverything(t *testing.T) {
	c, err := Load(env(map[string]string{
		"ORIGINCLOUD_PROJECT":         "projects/my-project",
		"ORIGINCLOUD_ZONE_IDS":        "z-1, projects/my-project/hostedZones/z-2,",
		"ORIGINCLOUD_ZONE_VISIBILITY": "Public",
		"DOMAIN_FILTER":               "example.com,example.org",
		"EXCLUDE_DOMAIN_FILTER":       "internal.example.com",
		"DRY_RUN":                     "true",
		"LOG_LEVEL":                   "debug",
		"LOG_FORMAT":                  "json",
		"WEBHOOK_ADDRESS":             "127.0.0.1:9999",
		"HEALTH_ADDRESS":              ":9090",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Project != "projects/my-project" || !slices.Equal(c.ZoneIDs, []string{"z-1", "z-2"}) || c.Visibility != VisibilityPublic ||
		!slices.Equal(c.DomainFilter, []string{"example.com", "example.org"}) || !slices.Equal(c.ExcludeDomains, []string{"internal.example.com"}) ||
		!c.DryRun || c.LogLevel != slog.LevelDebug || c.LogFormat != "json" || c.WebhookAddress != "127.0.0.1:9999" || c.HealthAddress != ":9090" {
		t.Errorf("config: %+v", c)
	}
}

func TestRegex(t *testing.T) {
	c, err := Load(env(map[string]string{
		"ORIGINCLOUD_PROJECT":            "p",
		"REGEXP_DOMAIN_FILTER":           `example\.(com|org)$`,
		"REGEXP_DOMAIN_FILTER_EXCLUSION": `^internal\.`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.RegexDomainFilter.MatchString("www.example.org") || !c.RegexDomainExclusion.MatchString("internal.example.com") {
		t.Errorf("config: %+v", c)
	}
}

func TestInvalid(t *testing.T) {
	tests := []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{}, "ORIGINCLOUD_PROJECT"},
		{map[string]string{"ORIGINCLOUD_PROJECT": "projects/a/b"}, "ORIGINCLOUD_PROJECT"},
		{map[string]string{"ORIGINCLOUD_PROJECT": "p", "ORIGINCLOUD_ZONE_IDS": "projects/other/hostedZones/z-1"}, "ORIGINCLOUD_ZONE_IDS"},
		{map[string]string{"ORIGINCLOUD_PROJECT": "p", "ORIGINCLOUD_ZONE_VISIBILITY": "internal"}, "ORIGINCLOUD_ZONE_VISIBILITY"},
		{map[string]string{"ORIGINCLOUD_PROJECT": "p", "DRY_RUN": "maybe"}, "DRY_RUN"},
		{map[string]string{"ORIGINCLOUD_PROJECT": "p", "LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		{map[string]string{"ORIGINCLOUD_PROJECT": "p", "LOG_FORMAT": "xml"}, "LOG_FORMAT"},
		{map[string]string{"ORIGINCLOUD_PROJECT": "p", "REGEXP_DOMAIN_FILTER": "("}, "REGEXP_DOMAIN_FILTER"},
		{map[string]string{"ORIGINCLOUD_PROJECT": "p", "REGEXP_DOMAIN_FILTER_EXCLUSION": "x"}, "needs REGEXP_DOMAIN_FILTER"},
		{map[string]string{"ORIGINCLOUD_PROJECT": "p", "REGEXP_DOMAIN_FILTER": "x", "DOMAIN_FILTER": "example.com"}, "one or the other"},
	}
	for _, tt := range tests {
		if _, err := Load(env(tt.vars)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Load(%v) = %v, want an error naming %s", tt.vars, err, tt.want)
		}
	}
}
