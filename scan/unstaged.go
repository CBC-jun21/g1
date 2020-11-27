package scan

import (
	"bytes"
	"fmt"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/sergi/go-diff/diffmatchpatch"
	"io"
	"time"
)

type UnstagedScanner struct {
	BaseScanner
	repo *git.Repository

	leaks []Leak
}

func NewUnstagedScanner(base BaseScanner, repo *git.Repository) *UnstagedScanner {
	return &UnstagedScanner{
		BaseScanner: base,
		repo: repo,
	}
}

func (us *UnstagedScanner) Scan() error {
	r, err := us.repo.Head()
	if err == plumbing.ErrReferenceNotFound {
		wt, err := us.repo.Worktree()
		if err != nil {
			return err
		}

		status, err := wt.Status()
		if err != nil {
			return err
		}
		for fn := range status {
			workTreeBuf := bytes.NewBuffer(nil)
			workTreeFile, err := wt.Filesystem.Open(fn)
			if err != nil {
				continue
			}
			if _, err := io.Copy(workTreeBuf, workTreeFile); err != nil {
				return err
			}
			us.leaks = append(us.leaks, checkRules(us.cfg, "", workTreeFile.Name(), emptyCommit(), workTreeBuf.String())...)
		}
		return nil
	} else if err != nil {
		return err
	}

	c, err := us.repo.CommitObject(r.Hash())
	if err != nil {
		return err
	}
	// Staged change so the Commit details do not yet exist. Insert empty defaults.
	c.Hash = plumbing.Hash{}
	c.Message = "***STAGED CHANGES***"
	c.Author.Name = ""
	c.Author.Email = ""
	c.Author.When = time.Unix(0, 0).UTC()

	prevTree, err := c.Tree()
	if err != nil {
		return err
	}
	wt, err := us.repo.Worktree()
	if err != nil {
		return err
	}

	status, err := wt.Status()
	for fn, state := range status {
		var (
			prevFileContents string
			currFileContents string
			filename         string
		)

		if state.Staging != git.Untracked {
			if state.Staging == git.Deleted {
				// file in staging has been deleted, aka it is not on the filesystem
				// so the contents of the file are ""
				currFileContents = ""
			} else {
				workTreeBuf := bytes.NewBuffer(nil)
				workTreeFile, err := wt.Filesystem.Open(fn)
				if err != nil {
					continue
				}
				if _, err := io.Copy(workTreeBuf, workTreeFile); err != nil {
					return err
				}
				currFileContents = workTreeBuf.String()
				filename = workTreeFile.Name()
			}

			// get files at HEAD state
			prevFile, err := prevTree.File(fn)
			if err != nil {
				prevFileContents = ""

			} else {
				prevFileContents, err = prevFile.Contents()
				if err != nil {
					return err
				}
				if filename == "" {
					filename = prevFile.Name
				}
			}

			dmp := diffmatchpatch.New()
			diffs := dmp.DiffCleanupSemantic(dmp.DiffMain(prevFileContents, currFileContents, false))
			var diffContents string
			for _, d := range diffs {
				if d.Type == diffmatchpatch.DiffInsert {
					diffContents += fmt.Sprintf("%s\n", d.Text)
				}
			}
			us.leaks = append(us.leaks, checkRules(us.cfg, "", filename, c, diffContents)...)
		}
	}

	if err != nil {
		return err
	}
	return nil
}

func (us *UnstagedScanner) GetLeaks() []Leak {
	return us.leaks
}
