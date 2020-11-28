package scan

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	log "github.com/sirupsen/logrus"
	"github.com/zricethezav/gitleaks/v6/config"
	"github.com/zricethezav/gitleaks/v6/options"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	diffAddPrefix           = "+"
	diffLineSignature       = " @@"
	diffLineSignaturePrefix = "@@ "
	defaultLineNumber       = -1
	diffAddFilePrefix       = "+++ b"
	diffAddFilePrefixSlash  = "+++ b/"
)

func timeoutReached(ctx context.Context) bool {
	if ctx.Err() == context.DeadlineExceeded {
		return true
	}
	return false
}

func obtainCommit(repo *git.Repository, commitSha string) (*object.Commit, error) {
	if commitSha == "latest" {
		ref, err := repo.Head()
		if err != nil {
			return nil, err
		}
		commitSha = ref.Hash().String()
	}
	return repo.CommitObject(plumbing.NewHash(commitSha))
}

func getRepo(opts options.Options) (*git.Repository, error) {
	if opts.OpenLocal() {
		return git.PlainOpen(opts.RepoPath)
	}
	if opts.CheckUncommitted() {
		// open git repo from PWD
		dir, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		return git.PlainOpen(dir)
	}
	return cloneRepo(opts)
}

func cloneRepo(opts options.Options) (*git.Repository, error) {
	cloneOpts, err := opts.CloneOptions()
	if err != nil {
		return nil, err
	}
	log.Infof("cloning... %s", cloneOpts.URL)
	return git.Clone(memory.NewStorage(), nil, cloneOpts)
}

// depthReached checks if i meets the depth (--depth=) if set
func depthReached(i int, opts options.Options) bool {
	if opts.Depth != 0 && opts.Depth == i {
		log.Warnf("Exceeded depth limit (%d)", i)
		return true
	}
	return false
}

// emptyCommit generates an empty commit used for scanning uncommitted changes
func emptyCommit() *object.Commit {
	return &object.Commit{
		Hash:    plumbing.Hash{},
		Message: "***STAGED CHANGES***",
		Author: object.Signature{
			Name:  "",
			Email: "",
			When:  time.Unix(0, 0).UTC(),
		},
	}
}

func loadRepoConfig(repo *git.Repository) (config.Config, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return config.Config{}, err
	}
	var f billy.File
	f, _ = wt.Filesystem.Open(".gitleaks.toml")
	if f == nil {
		f, err = wt.Filesystem.Open("gitleaks.toml")
		if err != nil {
			return config.Config{}, fmt.Errorf("problem loading repo config: %v", err)
		}
	}
	defer f.Close()
	var tomlLoader config.TomlLoader
	_, err = toml.DecodeReader(f, &tomlLoader)
	if err != nil {
		return config.Config{}, err
	}

	return tomlLoader.Parse()
}

// howManyThreads will return a number 1-GOMAXPROCS which is the number
// of goroutines that will spawn during gitleaks execution
func howManyThreads(threads int) int {
	maxThreads := runtime.GOMAXPROCS(0)
	if threads == 0 {
		return 1
	} else if threads > maxThreads {
		log.Warnf("%d threads set too high, setting to system max, %d", threads, maxThreads)
		return maxThreads
	}
	return threads
}

func checkRules(cfg config.Config, repoName string, filePath string, commit *object.Commit, content string) []Leak {
	filename := filepath.Base(filePath)
	path := filepath.Dir(filePath)
	var leaks []Leak

	skipRuleLookup := make(map[string]bool)
	// First do simple rule checks based on filename
	if skipCheck(cfg, filename, path) {
		return leaks
	}

	for _, rule := range cfg.Rules {
		if skipRule(rule, filename, filePath) {
			skipRuleLookup[rule.Description] = true
			continue
		}

		// If it doesnt contain a Content regex then it is a filename regex match
		if !ruleContainRegex(rule) {
			leak := Leak{
				LineNumber: defaultLineNumber,
				Line:       "N/A",
				Offender:   "Filename/path offender: " + filename,
				Commit:     commit.Hash.String(),
				Repo:       repoName,
				Message:    commit.Message,
				Rule:       rule.Description,
				Author:     commit.Author.Name,
				Email:      commit.Author.Email,
				Date:       commit.Author.When,
				Tags:       strings.Join(rule.Tags, ", "),
				File:       filename,
				// Operation:  diffOpToString(bundle.Operation),
			}
			// logLeak(leak)
			leaks = append(leaks, leak)
		}
	}

	lineNumber := 0

	// more intensive
	for _, line := range strings.Split(content, "\n") {
		for _, rule := range cfg.Rules {
			if _, ok := skipRuleLookup[rule.Description]; ok {
				continue
			}

			offender := rule.Regex.FindString(line)
			if offender == "" {
				continue
			}

			// check entropy
			groups := rule.Regex.FindStringSubmatch(offender)
			if isAllowListed(line, append(rule.AllowList.Regexes, cfg.Allowlist.Regexes...)) {
				continue
			}
			if len(rule.Entropies) != 0 && !trippedEntropy(groups, rule) {
				continue
			}

			// 0 is a match for the full regex pattern
			if 0 < rule.ReportGroup && rule.ReportGroup < len(groups) {
				offender = groups[rule.ReportGroup]
			}

			leak := Leak{
				LineNumber: lineNumber,
				Line:       line,
				Offender:   offender,
				Commit:     commit.Hash.String(),
				Repo:       repoName,
				Message:    commit.Message,
				Rule:       rule.Description,
				Author:     commit.Author.Name,
				Email:      commit.Author.Email,
				Date:       commit.Author.When,
				Tags:       strings.Join(rule.Tags, ", "),
				File:       filePath,
			}
			// logLeak(leak)
			leaks = append(leaks, leak)
		}
		lineNumber++
	}
	return leaks
}

func logLeak(leak Leak) {
	var b []byte
	b, _ = json.MarshalIndent(leak, "", "	")
	fmt.Println(string(b))
}

// getLogOptions determines what log options are used when iterating through commits.
// It is similar to `git log {branch}`. Default behavior is to log ALL branches so
// gitleaks gets the full git history.
func logOptions(repo *git.Repository, opts options.Options) (*git.LogOptions, error) {
	var logOpts git.LogOptions
	const dateformat string = "2006-01-02"
	const timeformat string = "2006-01-02T15:04:05-0700"
	if opts.CommitFrom != "" {
		logOpts.From = plumbing.NewHash(opts.CommitFrom)
	}
	if opts.CommitSince != "" {
		if t, err := time.Parse(timeformat, opts.CommitSince); err == nil {
			logOpts.Since = &t
		} else if t, err := time.Parse(dateformat, opts.CommitSince); err == nil {
			logOpts.Since = &t
		} else {
			return nil, err
		}
	}
	if opts.CommitUntil != "" {
		if t, err := time.Parse(timeformat, opts.CommitUntil); err == nil {
			logOpts.Until = &t
		} else if t, err := time.Parse(dateformat, opts.CommitUntil); err == nil {
			logOpts.Until = &t
		} else {
			return nil, err
		}
	}
	if opts.Branch != "" {
		refs, err := repo.Storer.IterReferences()
		if err != nil {
			return nil, err
		}
		err = refs.ForEach(func(ref *plumbing.Reference) error {
			if ref.Name().IsTag() {
				return nil
			}
			// check heads first
			if ref.Name().String() == "refs/heads/"+opts.Branch {
				logOpts = git.LogOptions{
					From: ref.Hash(),
				}
				return nil
			} else if ref.Name().String() == "refs/remotes/origin/"+opts.Branch {
				logOpts = git.LogOptions{
					From: ref.Hash(),
				}
				return nil
			}
			return nil
		})
		if err != nil {
			return nil, err
		}

		if logOpts.From.IsZero() {
			return nil, fmt.Errorf("could not find branch %s", opts.Branch)
		}
		return &logOpts, nil
	}
	if !logOpts.From.IsZero() || logOpts.Since != nil || logOpts.Until != nil {
		return &logOpts, nil
	}
	return &git.LogOptions{All: true}, nil
}
func skipCheck(cfg config.Config, filename string, path string) bool {
	// We want to check if there is a allowlist for this file
	if len(cfg.Allowlist.Files) != 0 {
		for _, reFileName := range cfg.Allowlist.Files {
			if regexMatched(filename, reFileName) {
				log.Debugf("allowlisted file found, skipping scan of file: %s", filename)
				return true
			}
		}
	}

	// We want to check if there is a allowlist for this path
	if len(cfg.Allowlist.Paths) != 0 {
		for _, reFilePath := range cfg.Allowlist.Paths {
			if regexMatched(path, reFilePath) {
				log.Debugf("file in allowlisted path found, skipping scan of file: %s", filename)
				return true
			}
		}
	}
	return false
}

func skipRule(rule config.Rule, filename string, path string) bool {
	// For each rule we want to check filename allowlists
	if isAllowListed(filename, rule.AllowList.Files) || isAllowListed(path, rule.AllowList.Paths) {
		return true
	}

	// If it has fileNameRegex and it doesnt match we continue to next rule
	if ruleContainFileRegex(rule) && !regexMatched(filename, rule.File) {
		return true
	}

	// If it has filePathRegex and it doesnt match we continue to next rule
	if ruleContainPathRegex(rule) && !regexMatched(path, rule.Path) {
		return true
	}
	return false
}

// regexMatched matched an interface to a regular expression. The interface f can
// be a string type or go-git *object.File type.
func regexMatched(f interface{}, re *regexp.Regexp) bool {
	if re == nil {
		return false
	}
	switch f.(type) {
	case nil:
		return false
	case string:
		if re.FindString(f.(string)) != "" {
			return true
		}
		return false
	case *object.File:
		if re.FindString(f.(*object.File).Name) != "" {
			return true
		}
		return false
	}
	return false
}

// diffOpToString converts a fdiff.Operation to a string
func diffOpToString(operation fdiff.Operation) string {
	switch operation {
	case fdiff.Add:
		return "addition"
	case fdiff.Equal:
		return "equal"
	default:
		return "deletion"
	}
}

// trippedEntropy checks if a given capture group or offender falls in between entropy ranges
// supplied by a custom gitleaks configuration. Gitleaks do not check entropy by default.
func trippedEntropy(groups []string, rule config.Rule) bool {
	for _, e := range rule.Entropies {
		if len(groups) > e.Group {
			entropy := shannonEntropy(groups[e.Group])
			if entropy >= e.Min && entropy <= e.Max {
				return true
			}
		}
	}
	return false
}

// shannonEntropy calculates the entropy of data using the formula defined here:
// https://en.wiktionary.org/wiki/Shannon_entropy
// Another way to think about what this is doing is calculating the number of bits
// needed to on average encode the data. So, the higher the entropy, the more random the data, the
// more bits needed to encode that data.
func shannonEntropy(data string) (entropy float64) {
	if data == "" {
		return 0
	}

	charCounts := make(map[rune]int)
	for _, char := range data {
		charCounts[char]++
	}

	invLength := 1.0 / float64(len(data))
	for _, count := range charCounts {
		freq := float64(count) * invLength
		entropy -= freq * math.Log2(freq)
	}

	return entropy
}

// Checks if the given rule has a regex
func ruleContainRegex(rule config.Rule) bool {
	if rule.Regex == nil {
		return false
	}
	if rule.Regex.String() == "" {
		return false
	}
	return true
}

// Checks if the given rule has a file name regex
func ruleContainFileRegex(rule config.Rule) bool {
	if rule.File == nil {
		return false
	}
	if rule.File.String() == "" {
		return false
	}
	return true
}

// Checks if the given rule has a file path regex
func ruleContainPathRegex(rule config.Rule) bool {
	if rule.Path == nil {
		return false
	}
	if rule.Path.String() == "" {
		return false
	}
	return true
}

func isCommitAllowListed(commitHash string, allowlistedCommits []string) bool {
	for _, hash := range allowlistedCommits {
		if commitHash == hash {
			return true
		}
	}
	return false
}

func isAllowListed(target string, allowList []*regexp.Regexp) bool {
	if len(allowList) != 0 {
		for _, re := range allowList {
			if re.FindString(target) != "" {
				return true
			}
		}
	}
	return false

}

func optsToCommits(opts options.Options) ([]string, error) {
	if opts.Commits != "" {
		return strings.Split(opts.Commits, ","), nil
	}
	file, err := os.Open(opts.CommitsFile)
	if err != nil {
		return []string{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var commits []string
	for scanner.Scan() {
		commits = append(commits, scanner.Text())
	}
	return commits, nil
}
