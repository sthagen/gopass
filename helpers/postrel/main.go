// Copyright 2021 The gopass Authors. All rights reserved.
// Use of this source code is governed by the MIT license,
// that can be found in the LICENSE file.

// Postrel is a helper that's supposed to be run after a release has been completed.
// It will update the gopasspw.github.io website and create a new GitHub milestone.
// Since it depends on the artifacts generated by the autorelease GitHub action we
// can't run it as part of the release helper.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/google/go-github/v33/github"
	"github.com/gopasspw/gopass/pkg/fsutil"
	"golang.org/x/oauth2"
)

var verTmpl = `package main

import (
	"strings"

	"github.com/blang/semver/v4"
)

func getVersion() semver.Version {
	sv, err := semver.Parse(strings.TrimPrefix(version, "v"))
	if err == nil {
		return sv
	}

	return semver.Version{
		Major: {{ .Major }},
		Minor: {{ .Minor }},
		Patch: {{ .Patch }},
		Pre: []semver.PRVersion{
			{VersionStr: "git"},
		},
		Build: []string{"HEAD"},
	}
}
`

const logo = `
   __     _    _ _      _ _   ___   ___
 /'_ '\ /'_'\ ( '_'\  /'_' )/',__)/',__)
( (_) |( (_) )| (_) )( (_| |\__, \\__, \
'\__  |'\___/'| ,__/''\__,_)(____/(____/
( )_) |       | |
 \___/'       (_)
`

func main() {
	ctx := context.Background()

	fmt.Print(logo)
	fmt.Println()
	fmt.Println("🌟 Performing post-release cleanup.")

	curVer, err := versionFile()
	if err != nil {
		panic(err)
	}
	nextVer := curVer
	nextVer.IncrementPatch()

	htmlDir := "../gopasspw.github.io"
	if h := os.Getenv("GOPASS_HTMLDIR"); h != "" {
		htmlDir = h
	}

	// update gopass.pw
	fmt.Println("☝  Updating gopass.pw ...")
	if err := updateGopasspw(htmlDir, curVer); err != nil {
		fmt.Printf("Failed to update gopasspw.github.io: %s\n", err)
	}

	// only update gopasspw
	if len(os.Args) > 1 && os.Args[1] == "render" {
		fmt.Println("💎🙌 Done (render gopasspw only) 🚀🚀🚀🚀🚀🚀")

		return
	}

	mustCheckEnv()

	ghCl, err := newGHClient(ctx)
	if err != nil {
		panic(err)
	}

	fmt.Println()
	fmt.Printf("✅ Current version is: %s\n", curVer.String())
	fmt.Printf("✅ New version milestone will be: %s\n", nextVer.String())
	fmt.Printf("✅ Expecting HTML in: %s\n", htmlDir)
	fmt.Println()
	fmt.Println("❓ Do you want to continue? (press any key to continue or Ctrl+C to abort)")
	fmt.Scanln()

	// create a new GitHub milestone
	fmt.Println("☝  Creating new GitHub Milestone(s) ...")
	if err := ghCl.createMilestones(ctx, nextVer); err != nil {
		fmt.Printf("Failed to create GitHub milestones: %s\n", err)
	}

	// update gopass integrations
	ui, err := newIntegrationsUpdater(ghCl.client, curVer)
	if err != nil {
		fmt.Printf("Failed to create integrations updater: %s\n", err)
	} else {
		ui.update(ctx)
	}

	// send PRs to update gopass ports
	upd, err := newRepoUpdater(ghCl.client, curVer, os.Getenv("GITHUB_USER"), os.Getenv("GITHUB_FORK"))
	if err != nil {
		fmt.Printf("Failed to create repo updater: %s\n", err)
	} else {
		upd.update(ctx)
	}

	fmt.Println("💎🙌 Done 🚀🚀🚀🚀🚀🚀")
}

func mustCheckEnv() {
	want := []string{"GITHUB_TOKEN", "GITHUB_USER", "GITHUB_FORK"}
	for _, e := range want {
		if sv := os.Getenv(e); sv == "" {
			panic("Please set: " + fmt.Sprintf("%v", want))
		}
	}
}

type ghClient struct {
	client *github.Client
	org    string
	repo   string
}

func newGHClient(ctx context.Context) (*ghClient, error) {
	pat := os.Getenv("GITHUB_TOKEN")
	if pat == "" {
		return nil, fmt.Errorf("❌ Please set GITHUB_TOKEN")
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: pat},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	return &ghClient{
		client: client,
		org:    "gopasspw",
		repo:   "gopass",
	}, nil
}

func (g *ghClient) createMilestones(ctx context.Context, v semver.Version) error {
	ms, _, err := g.client.Issues.ListMilestones(ctx, g.org, g.repo, nil)
	if err != nil {
		return err
	}

	// create a milestone for the next patch version
	if err := g.createMilestone(ctx, v.String(), 1, ms); err != nil {
		return err
	}

	// create a milestone for the next+1 patch version
	v.IncrementPatch()
	if err := g.createMilestone(ctx, v.String(), 2, ms); err != nil {
		return err
	}

	// create a milestone for the next minor version
	v.IncrementMinor()
	v.Patch = 0

	return g.createMilestone(ctx, v.String(), 90, ms)
}

func (g *ghClient) createMilestone(ctx context.Context, title string, offset int, ms []*github.Milestone) error {
	for _, m := range ms {
		if *m.Title == title {
			fmt.Printf("❌ Milestone %s exists\n", title)

			return nil
		}
	}

	due := time.Now().Add(time.Duration(offset) * 30 * 24 * time.Hour)
	_, _, err := g.client.Issues.CreateMilestone(ctx, g.org, g.repo, &github.Milestone{
		Title: &title,
		DueOn: &due,
	})
	if err == nil {
		fmt.Printf("✅ Milestone %s created\n", title)
	}

	return err
}

func updateGopasspw(dir string, ver semver.Version) error {
	buf, err := os.ReadFile(filepath.Join(dir, "index.tpl"))
	if err != nil {
		return err
	}

	tmpl, err := template.New("index").Parse(string(buf))
	if err != nil {
		return err
	}

	fh, err := os.Create(filepath.Join(dir, "index.html"))
	if err != nil {
		return err
	}
	defer fh.Close()

	type pl struct {
		Version string
	}

	if err := tmpl.Execute(fh, pl{
		Version: ver.String(),
	}); err != nil {
		return err
	}

	return gitCommitAndPush(dir, fmt.Sprintf("v%s", ver))
}

func isGitClean(dir string) bool {
	cmd := exec.Command("git", "diff", "--stat")
	cmd.Dir = dir
	buf, err := cmd.CombinedOutput()
	if err != nil {
		panic(err)
	}

	if strings.TrimSpace(string(buf)) != "" {
		fmt.Printf("❌ Git in %s is not clean: %q\n", dir, string(buf))

		return false
	}

	return true
}

func gitCoMaster(dir string) error {
	cmd := exec.Command("git", "checkout", "master")
	cmd.Dir = dir
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func gitPom(dir string) error {
	cmd := exec.Command("git", "pull", "origin", "master")
	cmd.Dir = dir
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func gitCommitAndPush(dir, tag string) error {
	cmd := exec.Command("git", "commit", "-a", "-s", "-m", "Update to "+tag)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to commit changes: %w", err)
	}

	cmd = exec.Command("git", "push", "origin", "master")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to push changes: %w", err)
	}

	return nil
}

func gitTagAndPush(dir string, tag string) error {
	cmd := exec.Command("git", "tag", "-m", "'Tag "+tag+"'", tag)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to commit changes: %w", err)
	}

	cmd = exec.Command("git", "push", "origin", tag)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to push changes: %w", err)
	}

	return nil
}

func gitHasTag(dir string, tag string) bool {
	cmd := exec.Command("git", "rev-parse", tag)
	cmd.Dir = dir

	return cmd.Run() == nil
}

func runCmd(dir string, args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func versionFile() (semver.Version, error) {
	buf, err := os.ReadFile("VERSION")
	if err != nil {
		return semver.Version{}, err
	}

	return semver.Parse(strings.TrimSpace(string(buf)))
}

func goVersion() string {
	sv := semver.MustParse(strings.TrimPrefix(runtime.Version(), "go"))

	return fmt.Sprintf("%d.%d", sv.Major, sv.Minor)
}

type inUpdater struct {
	github *github.Client
	v      semver.Version
	goVer  string // go version as major.minor (for use in go.mod and GH workflows)
}

func newIntegrationsUpdater(client *github.Client, v semver.Version) (*inUpdater, error) {
	return &inUpdater{
		github: client,
		v:      v,
		goVer:  goVersion(),
	}, nil
}

func (u *inUpdater) update(ctx context.Context) {
	for _, upd := range []string{
		"git-credential-gopass",
		"gopass-hibp",
		"gopass-jsonapi",
		"gopass-summon-provider",
	} {
		fmt.Println()
		fmt.Println("------------------------------")
		fmt.Println()
		fmt.Printf("🌟 Updating: %s ...\n", upd)
		fmt.Println()
		if err := u.doUpdate(ctx, upd); err != nil {
			fmt.Printf("❌ Updating %s failed: %s\n", upd, err)

			continue
		}
		fmt.Printf("✅ Integration %s is up to date.\n", upd)
	}
}

func (u *inUpdater) doUpdate(ctx context.Context, dir string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(cwd), dir)

	tag := fmt.Sprintf("v%s", u.v.String())
	// check if the release is already tagged
	if gitHasTag(path, tag) {
		fmt.Printf("✅ Integration %s has tag %s already.\n", dir, tag)

		// TODO return nil
	}
	fmt.Printf("✅ [%s] %s is not tagged, yet.\n", dir, tag)

	// make sure we're at head
	if !isGitClean(path) {
		return fmt.Errorf("git not clean at %s", path)
	}
	fmt.Printf("✅ [%s] Git is clean.", dir)

	// git pull origin master
	if err := gitPom(path); err != nil {
		return fmt.Errorf("failed to fetch changes at %s: %s", path, err)
	}

	// make upgrade
	if err := runCmd(path, "make", "upgrade"); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] make upgrade.\n", dir)

	// go get github.com/gopasspw/gopass@tag
	if err := runCmd(path, "go", "get", "github.com/gopasspw/gopass@"+tag); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] updated gopass dependency.\n", dir)

	// sync .golangci.yml ?
	if err := fsutil.CopyFile(filepath.Join(cwd, ".golangci.yml"), filepath.Join(path, ".golangci.yml")); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] synced .golangci.yml.\n", dir)

	// update go.mod
	if err := runCmd(path, "go", "mod", "edit", "-go="+u.goVer); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] updated Go version in go.mod to %s.\n", dir, u.goVer)

	// update workflows
	if err := u.updateWorkflows(ctx, path); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] updated workflows.\n", dir)

	// update VERSION
	if err := os.WriteFile(filepath.Join(path, "VERSION"), []byte(u.v.String()+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] wrote VERSION.\n", dir)

	// update version.go
	if err := u.writeVersionGo(path); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] wrote version.go.\n", dir)

	// update CHANGELOG.md
	if err := u.updateChangelog(ctx, path); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] wrote CHANGELOG.md.\n", dir)

	// git commit
	if err := gitCommitAndPush(path, tag); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] committed.\n", dir)

	// git tag v
	if err := gitTagAndPush(path, tag); err != nil {
		return err
	}
	fmt.Printf("✅ [%s] tagged.\n", dir)

	return nil
}

func (u *inUpdater) updateWorkflows(ctx context.Context, dir string) error {
	filepath.Walk(filepath.Join(dir, ".github", "workflows"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			fmt.Printf("Failed to walk %s: %s\n", path, err)

			return nil
		}
		if info.IsDir() {
			// fmt.Printf("Skipping dir %s\n", path)

			return nil
		}
		if !strings.HasSuffix(path, ".yml") {
			// fmt.Printf("Skipping file %s\n", path)

			return nil
		}

		return u.updateWorkflow(ctx, path)
	})

	return nil
}

var goVersionRE = regexp.MustCompile(`go-version:\s+\d+\.\d+`)

func (u *inUpdater) updateWorkflow(ctx context.Context, path string) error {
	buf, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	str := goVersionRE.ReplaceAllString(string(buf), "go-version: "+u.goVer)
	// no change, no write
	if str == string(buf) {
		// fmt.Printf("No changes in %s\n", path)

		return nil
	}

	fmt.Printf("Wrote %s\n", path)

	return os.WriteFile(path, []byte(str), 0o644)
}

type tplPayload struct {
	Major uint64
	Minor uint64
	Patch uint64
}

func (u *inUpdater) writeVersionGo(path string) error {
	tmpl, err := template.New("version").Parse(verTmpl)
	if err != nil {
		return err
	}

	fn := filepath.Join(path, "version.go")
	fh, err := os.Create(fn)
	if err != nil {
		return err
	}
	defer fh.Close()

	return tmpl.Execute(fh, tplPayload{
		Major: u.v.Major,
		Minor: u.v.Minor,
		Patch: u.v.Patch,
	})
}

func (u *inUpdater) updateChangelog(ctx context.Context, dir string) error {
	fn := filepath.Join(dir, "CHANGELOG.md")

	buf, err := os.ReadFile(fn)
	if err != nil {
		return err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s\n", u.v.String())
	fmt.Fprintln(&sb)
	fmt.Fprintf(&sb, "- Bump dependencies to gopass release v%s\n", u.v.String())
	fmt.Fprintln(&sb)

	_, err = sb.Write(buf)
	if err != nil {
		return err
	}

	if err := os.WriteFile(fn, []byte(sb.String()), 0o644); err != nil {
		return err
	}

	return nil
}

type repoUpdater struct {
	github    *github.Client
	ghFork    string
	ghUser    string
	v         semver.Version
	relURL    string
	arcURL    string
	relSHA256 string
	relSHA512 string
	arcSHA256 string
	arcSHA512 string
}

func newRepoUpdater(client *github.Client, v semver.Version, user, fork string) (*repoUpdater, error) {
	relURL := fmt.Sprintf("https://github.com/gopasspw/gopass/releases/download/v%s/gopass-%s.tar.gz", v.String(), v.String())
	// fetch https://github.com/gopasspw/gopass/archive/vVER.tar.gz
	// compute sha256, sha512
	relSHA256, relSHA512, err := checksum(relURL)
	if err != nil {
		return nil, err
	}
	arcURL := fmt.Sprintf("https://github.com/gopasspw/gopass/archive/v%s.tar.gz", v.String())
	// fetch https://github.com/gopasspw/gopass/archive/vVER.tar.gz
	// compute sha256, sha512
	arcSHA256, arcSHA512, err := checksum(arcURL)
	if err != nil {
		return nil, err
	}

	return &repoUpdater{
		github:    client,
		ghFork:    fork,
		ghUser:    user,
		v:         v,
		relURL:    relURL,
		arcURL:    arcURL,
		relSHA256: relSHA256,
		relSHA512: relSHA512,
		arcSHA256: arcSHA256,
		arcSHA512: arcSHA512,
	}, nil
}

func (u *repoUpdater) update(ctx context.Context) {
	for _, upd := range []struct {
		Distro string
		UpFn   func(context.Context) error
	}{
		// {
		// 	Distro: "AlpineLinux",
		// 	UpFn:   u.updateAlpine,
		// },
	} {
		fmt.Println()
		fmt.Println("------------------------------")
		fmt.Println()
		fmt.Printf("🌟 Updating: %s ...\n", upd.Distro)
		fmt.Println()
		if err := upd.UpFn(ctx); err != nil {
			fmt.Printf("❌ Updating %s failed: %s\n", upd.Distro, err)

			continue
		}
		fmt.Printf("✅ Distro %s updated\n", upd.Distro)
	}
}

func (u *repoUpdater) updateAlpine(ctx context.Context) error {
	dir := "../repos/alpine/"
	if d := os.Getenv("GOPASS_ALPINE_PKG_DIR"); d != "" {
		dir = d
	}

	r := &repo{
		ver: u.v,
		url: u.arcURL,
		dir: dir,
		msg: "community/gopass: upgrade to " + u.v.String(),
		rem: u.ghFork,
	}

	if err := r.updatePrepare(); err != nil {
		return err
	}
	fmt.Println("✅ Prepared")

	// update community/gopass/APKBUILD
	buildFn := "community/gopass/APKBUILD"
	buildPath := filepath.Join(dir, buildFn)

	repl := map[string]*string{
		"pkgver=":     strp("pkgver=" + u.v.String()),
		"sha512sums=": strp("sha512sums=\"" + u.arcSHA512 + "  gopass-" + u.v.String() + ".tar.gz\""),
		"source=":     strp(`source="$pkgname-$pkgver.tar.gz::https://github.com/gopasspw/gopass/archive/v$pkgver.tar.gz"`),
	}

	if err := updateBuild(buildPath, repl); err != nil {
		return err
	}
	fmt.Println("✅ Built")

	if err := r.updateFinalize(buildFn); err != nil {
		return err
	}
	fmt.Println("✅ Finalized")

	// TODO could open an MR: https://docs.gitlab.com/ce/api/merge_requests.html#create-mhttps://docs.gitlab.com/ce/api/merge_requests.html#comments-on-merge-requestsr
	return nil
}

func (u *repoUpdater) updateHomebrew(ctx context.Context) error {
	dir := "../repos/homebrew/"
	if d := os.Getenv("GOPASS_HOMEBREW_PKG_DIR"); d != "" {
		dir = d
	}

	r := &repo{
		ver: u.v,
		url: u.relURL,
		dir: dir,
		rem: u.ghFork,
	}

	if err := r.updatePrepare(); err != nil {
		return err
	}
	fmt.Println("✅ Prepared")

	// update Formula/gopass.rb
	buildFn := "Formula/gopass.rb"
	buildPath := filepath.Join(dir, buildFn)

	repl := map[string]*string{
		"url \"https://github.com/": strp("url \"" + u.relURL + "\""),
		"sha256 \"":                 strp("sha256 \"" + u.relSHA256 + "\""),
	}
	if err := updateBuild(
		buildPath,
		repl,
	); err != nil {
		return err
	}
	fmt.Println("✅ Built")

	if err := r.updateFinalize(buildFn); err != nil {
		return err
	}
	fmt.Println("✅ Finalized")

	return u.createPR(ctx, r.commitMsg(), u.ghUser+":"+r.branch(), "Homebrew", "homebrew-core")
}

func (u *repoUpdater) updateVoid(ctx context.Context) error {
	dir := "../repos/void/"
	if d := os.Getenv("GOPASS_VOID_PKG_DIR"); d != "" {
		dir = d
	}

	r := &repo{
		ver: u.v,
		url: u.arcURL,
		dir: dir,
		rem: u.ghFork,
	}

	if err := r.updatePrepare(); err != nil {
		return err
	}
	fmt.Println("✅ Prepared")

	// update srcpkgs/gopass/template
	buildFn := "srcpkgs/gopass/template"
	buildPath := filepath.Join(dir, buildFn)

	repl := map[string]*string{
		"version=":   strp("version=" + u.v.String()),
		"checksum=":  strp("checksum=" + u.arcSHA256),
		"distfiles=": strp(`distfiles="https://github.com/gopasspw/gopass/archive/v${version}.tar.gz"`),
	}
	if err := updateBuild(
		buildPath,
		repl,
	); err != nil {
		return err
	}
	fmt.Println("✅ Built")

	if err := r.updateFinalize(buildFn); err != nil {
		return err
	}
	fmt.Println("✅ Finalized")

	return u.createPR(ctx, r.commitMsg(), u.ghUser+":"+r.branch(), "void-linux", "void-packages")
}

func (u *repoUpdater) createPR(ctx context.Context, title, from, toOrg, toRepo string) error {
	newPR := &github.NewPullRequest{
		Title:               github.String(title),
		Head:                github.String(from),
		Base:                github.String("master"),
		Body:                github.String(title),
		MaintainerCanModify: github.Bool(true),
	}

	pr, resp, err := u.github.PullRequests.Create(ctx, toOrg, toRepo, newPR)
	if err != nil {
		fmt.Printf("❌ Creating GitHub PR failed: %s", err)
		fmt.Printf("Request: %+v\n", newPR)
		fmt.Printf("Response: %+v\n", resp)

		return err
	}
	fmt.Printf("✅ GitHub PR created: %s\n", pr.GetHTMLURL())

	return err
}

func checksum(url string) (string, string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	s2 := sha256.New()
	s5 := sha512.New()
	w := io.MultiWriter(s2, s5)

	_, err = io.Copy(w, resp.Body)
	if err != nil {
		return "", "", err
	}

	return fmt.Sprintf("%x", s2.Sum(nil)), fmt.Sprintf("%x", s5.Sum(nil)), nil
}

func updateBuild(path string, m map[string]*string) error {
	fin, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fin.Close()

	npath := path + ".new"
	fout, err := os.Create(npath)
	if err != nil {
		return err
	}
	defer fout.Close()

	s := bufio.NewScanner(fin)
SCAN:
	for s.Scan() {
		line := s.Text()
		for match, repl := range m {
			if strings.HasPrefix(line, match) {
				if repl != nil {
					fmt.Fprintln(fout, *repl)
				}

				continue SCAN
			}
		}
		fmt.Fprintln(fout, line)
	}

	return os.Rename(npath, path)
}

type repo struct {
	ver semver.Version // gopass version
	url string         // gopass download url
	dir string         // repo dir
	msg string
	rem string // remote
}

func (r *repo) branch() string {
	return fmt.Sprintf("gopass-%s", r.ver.String())
}

func (r *repo) commitMsg() string {
	if r.msg != "" {
		return r.msg
	}

	return "gopass: update to " + r.ver.String() + "\nNote: This is an auto-generated change as part of the gopass release process.\n"
}

func (r *repo) updatePrepare() error {
	fmt.Println("🌟 Running prepare ...")

	// git co master
	if err := r.gitCoMaster(); err != nil {
		return fmt.Errorf("git checkout master failed: %w", err)
	}
	if !r.isGitClean() {
		return fmt.Errorf("git is dirty")
	}
	// git pull origin master
	if err := r.gitPom(); err != nil {
		return fmt.Errorf("git pull origin master failed: %w", err)
	}
	// git co -b gopass-VER
	if err := r.gitBranch(); err == nil {
		return nil
	}

	// git branch -d gopass-VER
	if err := r.gitBranchDel(); err != nil {
		return fmt.Errorf("git branch -d failed: %w", err)
	}

	return r.gitBranch()
}

func (r *repo) updateFinalize(path string) error {
	fmt.Println("🌟 Running finalize ...")

	// git commit -m 'gopass: update to VER'
	if err := r.gitCommit(path); err != nil {
		return fmt.Errorf("git commit %s failed: %w", path, err)
	}
	// git push myfork gopass-VER
	return r.gitPush(r.rem)
}

func (r *repo) gitCoMaster() error {
	cmd := exec.Command("git", "checkout", "master")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = r.dir
	fmt.Printf("Running command: %s\n", cmd)

	return cmd.Run()
}

func (r *repo) gitBranch() error {
	cmd := exec.Command("git", "checkout", "-b", r.branch())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = r.dir
	fmt.Printf("Running command: %s\n", cmd)

	return cmd.Run()
}

func (r *repo) gitBranchDel() error {
	cmd := exec.Command("git", "branch", "-D", r.branch())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = r.dir
	fmt.Printf("Running command: %s\n", cmd)

	return cmd.Run()
}

func (r *repo) gitPom() error {
	cmd := exec.Command("git", "pull", "origin", "master")
	// hide long pull output unless an error occurs
	buf := &bytes.Buffer{}
	cmd.Stdout = buf
	cmd.Stderr = os.Stderr
	cmd.Dir = r.dir
	if err := cmd.Run(); err != nil {
		fmt.Println(buf.String())

		return err
	}

	return nil
}

func (r *repo) gitPush(remote string) error {
	cmd := exec.Command("git", "push", remote, r.branch())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = r.dir
	fmt.Printf("Running command: %s\n", cmd)

	return cmd.Run()
}

func (r *repo) gitCommit(files ...string) error {
	args := []string{"add"}
	args = append(args, files...)

	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = r.dir
	fmt.Printf("Running command: %s\n", cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	cmd = exec.Command("git", "commit", "-s", "-m", r.commitMsg())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = r.dir
	fmt.Printf("Running command: %s\n", cmd)

	return cmd.Run()
}

func (r *repo) isGitClean() bool {
	cmd := exec.Command("git", "diff", "--stat")
	cmd.Dir = r.dir

	buf, err := cmd.CombinedOutput()
	if err != nil {
		panic(err)
	}

	return strings.TrimSpace(string(buf)) == ""
}

func strp(s string) *string {
	return &s
}
