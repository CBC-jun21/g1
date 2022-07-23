package config

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
)

//go:embed gitleaks.toml
var DefaultConfig string

// lil bit of state never killed nobody
var extendDepth int

const maxExtendDepth = 2

// ViperConfig is the config struct used by the Viper config package
// to parse the config file. This struct does not include regular expressions.
// It is used as an intermediary to convert the Viper config to the Config struct.
type ViperConfig struct {
	Description string
	Extends     Extends
	Rules       []struct {
		ID          string
		Description string
		Entropy     float64
		SecretGroup int
		Regex       string
		Keywords    []string
		Path        string
		Tags        []string

		Allowlist struct {
			Regexes   []string
			Paths     []string
			Commits   []string
			StopWords []string
		}
	}
	Allowlist struct {
		Regexes   []string
		Paths     []string
		Commits   []string
		StopWords []string
	}
}

// Config is a configuration struct that contains rules and an allowlist if present.
type Config struct {
	Extends     Extends
	Path        string
	Description string
	Rules       map[string]Rule
	Allowlist   Allowlist
	Keywords    []string
}

// Extends is a struct that allows users to define how they want their
// configuration extended by other configuration files.
type Extends struct {
	Path       string
	URL        string
	UseDefault bool
}

func (vc *ViperConfig) Translate() (Config, error) {
	var keywords []string
	rulesMap := make(map[string]Rule)

	for _, r := range vc.Rules {
		var allowlistRegexes []*regexp.Regexp
		for _, a := range r.Allowlist.Regexes {
			allowlistRegexes = append(allowlistRegexes, regexp.MustCompile(a))
		}
		var allowlistPaths []*regexp.Regexp
		for _, a := range r.Allowlist.Paths {
			allowlistPaths = append(allowlistPaths, regexp.MustCompile(a))
		}

		if r.Keywords == nil {
			r.Keywords = []string{}
		} else {
			for _, k := range r.Keywords {
				keywords = append(keywords, strings.ToLower(k))
			}
		}

		if r.Tags == nil {
			r.Tags = []string{}
		}

		var configRegex *regexp.Regexp
		var configPathRegex *regexp.Regexp
		if r.Regex == "" {
			configRegex = nil
		} else {
			configRegex = regexp.MustCompile(r.Regex)
		}
		if r.Path == "" {
			configPathRegex = nil
		} else {
			configPathRegex = regexp.MustCompile(r.Path)
		}
		r := Rule{
			Description: r.Description,
			RuleID:      r.ID,
			Regex:       configRegex,
			Path:        configPathRegex,
			SecretGroup: r.SecretGroup,
			Entropy:     r.Entropy,
			Tags:        r.Tags,
			Keywords:    r.Keywords,
			Allowlist: Allowlist{
				Regexes:   allowlistRegexes,
				Paths:     allowlistPaths,
				Commits:   r.Allowlist.Commits,
				StopWords: r.Allowlist.StopWords,
			},
		}
		if r.Regex != nil && r.SecretGroup > r.Regex.NumSubexp() {
			return Config{}, fmt.Errorf("%s invalid regex secret group %d, max regex secret group %d", r.Description, r.SecretGroup, r.Regex.NumSubexp())
		}
		rulesMap[r.RuleID] = r
		// rules = append(rules, r)
	}
	var allowlistRegexes []*regexp.Regexp
	for _, a := range vc.Allowlist.Regexes {
		allowlistRegexes = append(allowlistRegexes, regexp.MustCompile(a))
	}
	var allowlistPaths []*regexp.Regexp
	for _, a := range vc.Allowlist.Paths {
		allowlistPaths = append(allowlistPaths, regexp.MustCompile(a))
	}
	c := Config{
		Description: vc.Description,
		Extends:     vc.Extends,
		Rules:       rulesMap,
		Allowlist: Allowlist{
			Regexes:   allowlistRegexes,
			Paths:     allowlistPaths,
			Commits:   vc.Allowlist.Commits,
			StopWords: vc.Allowlist.StopWords,
		},
		Keywords: keywords,
	}

	if maxExtendDepth != extendDepth {
		// if the user supplied
		if c.Extends.UseDefault {
			c.extendDefault()
		} else if c.Extends.Path != "" {
			c.extendPath()
		}

	}

	return c, nil
}

func (c *Config) extendDefault() {
	extendDepth++
	viper.SetConfigType("toml")
	if err := viper.ReadConfig(strings.NewReader(DefaultConfig)); err != nil {
		log.Fatal().Msgf("err reading toml %s", err.Error())
	}
	defaultViperConfig := ViperConfig{}
	if err := viper.Unmarshal(&defaultViperConfig); err != nil {
		log.Fatal().Err(err).Msg("Failed to load config")
	}
	cfg, err := defaultViperConfig.Translate()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load config")
	}
	log.Debug().Msg("extending config with default config")
	c.extend(cfg)

}

func (c *Config) extendPath() {
	extendDepth++
	viper.SetConfigFile(c.Extends.Path)
	if err := viper.ReadInConfig(); err != nil {
		log.Fatal().Msgf("Unable to load gitleaks config, err: %s", err)
	}
	extensionViperConfig := ViperConfig{}
	if err := viper.Unmarshal(&extensionViperConfig); err != nil {
		log.Fatal().Err(err).Msg("Failed to load config")
	}
	cfg, err := extensionViperConfig.Translate()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load config")
	}
	log.Debug().Msgf("extending config with %s", c.Extends.Path)
	c.extend(cfg)
}

func (c *Config) extendURL() {
	// TODO
}

func (c *Config) extend(extensionConfig Config) {
	// iterate through default rules and add whichever rules
	// are not in the base config rules but are in the default rules
	for ruleID, rule := range extensionConfig.Rules {
		if _, ok := c.Rules[ruleID]; !ok {
			c.Rules[ruleID] = rule
			c.Keywords = append(c.Keywords, rule.Keywords...)
		}
	}

	// append allowlists, not attempting to merge
	c.Allowlist.Commits = append(c.Allowlist.Commits,
		extensionConfig.Allowlist.Commits...)
	c.Allowlist.Paths = append(c.Allowlist.Paths,
		extensionConfig.Allowlist.Paths...)
	c.Allowlist.Regexes = append(c.Allowlist.Regexes,
		extensionConfig.Allowlist.Regexes...)
}
