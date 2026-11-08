// Package config reads the webhook's configuration from the environment.
//
// The credentials aren't read here: the Go SDK takes them from
// ORIGINCLOUD_CLIENT_ID and ORIGINCLOUD_CLIENT_SECRET, or
// ORIGINCLOUD_ACCESS_TOKEN, itself.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
)

// Defaults for the listen addresses. The webhook listens on localhost only,
// for the external-dns container of the same pod; the health endpoints
// listen on every interface, for the kubelet's probes.
const (
	DefaultWebhookAddress = "localhost:8888"
	DefaultHealthAddress  = ":8080"
)

// Visibility selects public zones, private zones, or both.
type Visibility string

// The visibilities ORIGINCLOUD_ZONE_VISIBILITY takes.
const (
	VisibilityAny     Visibility = ""
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

// Config is the webhook's configuration.
type Config struct {
	// Project is the project whose zones are managed, as a resource name,
	// projects/{project}.
	Project string
	// ZoneIDs, when not empty, are the only zones managed, by id.
	ZoneIDs []string
	// Visibility, when set, limits the zones managed to public or private
	// ones.
	Visibility Visibility

	// DomainFilter and ExcludeDomains are the domains managed, and the
	// domains below them that aren't, as external-dns's --domain-filter
	// and --exclude-domains take them.
	DomainFilter   []string
	ExcludeDomains []string
	// RegexDomainFilter and RegexDomainExclusion select the domains
	// managed by regular expression instead.
	RegexDomainFilter    *regexp.Regexp
	RegexDomainExclusion *regexp.Regexp

	// DryRun logs the changes instead of making them.
	DryRun bool

	LogLevel slog.Level
	// LogFormat is "text" or "json".
	LogFormat string

	// WebhookAddress is where the webhook API listens.
	WebhookAddress string
	// HealthAddress is where /healthz and /readyz listen.
	HealthAddress string
}

// Load reads the configuration from getenv, such as os.Getenv.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		LogFormat:      "text",
		WebhookAddress: DefaultWebhookAddress,
		HealthAddress:  DefaultHealthAddress,
	}
	var errs []error

	project, err := projectName(getenv("ORIGINCLOUD_PROJECT"))
	if err != nil {
		errs = append(errs, err)
	}
	c.Project = project

	for _, id := range list(getenv("ORIGINCLOUD_ZONE_IDS")) {
		zoneID, err := zoneID(c.Project, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.ZoneIDs = append(c.ZoneIDs, zoneID)
	}

	switch v := Visibility(strings.ToLower(strings.TrimSpace(getenv("ORIGINCLOUD_ZONE_VISIBILITY")))); v {
	case VisibilityAny, VisibilityPublic, VisibilityPrivate:
		c.Visibility = v
	default:
		errs = append(errs, fmt.Errorf("ORIGINCLOUD_ZONE_VISIBILITY must be public or private, got %q", v))
	}

	c.DomainFilter = list(getenv("DOMAIN_FILTER"))
	c.ExcludeDomains = list(getenv("EXCLUDE_DOMAIN_FILTER"))
	if c.RegexDomainFilter, err = regex("REGEXP_DOMAIN_FILTER", getenv); err != nil {
		errs = append(errs, err)
	}
	if c.RegexDomainExclusion, err = regex("REGEXP_DOMAIN_FILTER_EXCLUSION", getenv); err != nil {
		errs = append(errs, err)
	}
	if c.RegexDomainExclusion != nil && c.RegexDomainFilter == nil {
		errs = append(errs, errors.New("REGEXP_DOMAIN_FILTER_EXCLUSION needs REGEXP_DOMAIN_FILTER"))
	}
	if c.RegexDomainFilter != nil && (len(c.DomainFilter) > 0 || len(c.ExcludeDomains) > 0) {
		errs = append(errs, errors.New("REGEXP_DOMAIN_FILTER replaces DOMAIN_FILTER and EXCLUDE_DOMAIN_FILTER: set one or the other"))
	}

	if c.DryRun, err = boolean("DRY_RUN", getenv); err != nil {
		errs = append(errs, err)
	}

	if level := getenv("LOG_LEVEL"); level != "" {
		if err := c.LogLevel.UnmarshalText([]byte(level)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", level))
		}
	}
	switch format := strings.ToLower(getenv("LOG_FORMAT")); format {
	case "":
	case "text", "json":
		c.LogFormat = format
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be text or json, got %q", format))
	}

	if address := getenv("WEBHOOK_ADDRESS"); address != "" {
		c.WebhookAddress = address
	}
	if address := getenv("HEALTH_ADDRESS"); address != "" {
		c.HealthAddress = address
	}

	return c, errors.Join(errs...)
}

// projectName takes a project id or name and returns its name.
func projectName(value string) (string, error) {
	id := strings.TrimPrefix(strings.TrimSpace(value), "projects/")
	switch {
	case id == "":
		return "", errors.New("ORIGINCLOUD_PROJECT must name the project whose zones to manage")
	case strings.Contains(id, "/"):
		return "", fmt.Errorf("ORIGINCLOUD_PROJECT must be a project id or projects/{project}, got %q", value)
	}
	return "projects/" + id, nil
}

// zoneID takes a zone id or a zone's resource name in project, and returns
// its id.
func zoneID(project, value string) (string, error) {
	if !strings.Contains(value, "/") {
		return value, nil
	}
	id, ok := strings.CutPrefix(value, project+"/hostedZones/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", fmt.Errorf("ORIGINCLOUD_ZONE_IDS takes zone ids, or names of zones in %s, got %q", project, value)
	}
	return id, nil
}

// list splits a comma-separated value, dropping empty entries.
func list(value string) []string {
	var out []string
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func regex(name string, getenv func(string) string) (*regexp.Regexp, error) {
	value := getenv(name)
	if value == "" {
		return nil, nil
	}
	re, err := regexp.Compile(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return re, nil
}

func boolean(name string, getenv func(string) string) (bool, error) {
	value := getenv(name)
	if value == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", name, value)
	}
	return b, nil
}
