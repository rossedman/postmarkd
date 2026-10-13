// Command site builds postmarkd's documentation website from the Markdown in
// docs/ (plus a few top-level project files) and a hand-written landing page.
//
//	cd site && go run . -out ../_site
//
// All links in the output are relative, so the site works at any base path
// (GitHub Pages project sites, a local file:// preview, or a custom domain).
// The build fails on broken links between pages or to missing anchors.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed templates static
var embedded embed.FS

const repoURL = "https://github.com/rossedman/postmarkd"

// Section groups pages in the sidebar.
type Section struct {
	Title string
	Pages []*Page
}

// nav defines every page, in sidebar order. Source paths are relative to the
// repository root.
var nav = []struct {
	title string
	pages [][2]string // slug, source
}{
	{"Get started", [][2]string{
		{"getting-started", "docs/getting-started.md"},
		{"sending", "docs/sending.md"},
		{"testing", "docs/testing.md"},
	}},
	{"Guides", [][2]string{
		{"email-authentication", "docs/email-authentication.md"},
		{"containers", "docs/containers.md"},
		{"deployment", "docs/deployment.md"},
	}},
	{"Reference", [][2]string{
		{"configuration", "docs/configuration.md"},
		{"cli", "docs/cli.md"},
		{"api", "docs/api.md"},
		{"architecture", "docs/architecture.md"},
		{"benchmarks", "docs/benchmarks.md"},
	}},
	{"Project", [][2]string{
		{"changelog", "CHANGELOG.md"},
		{"contributing", "CONTRIBUTING.md"},
		{"code-of-conduct", "CODE_OF_CONDUCT.md"},
		{"security", "SECURITY.md"},
	}},
}

// navTitles shortens long page titles in the sidebar.
var navTitles = map[string]string{
	"email-authentication": "Email authentication",
	"containers":           "Containers & sidecars",
	"deployment":           "Deployment & operations",
	"testing":              "Testing",
	"contributing":         "Contributing",
	"code-of-conduct":      "Code of Conduct",
	"security":             "Security",
}

func main() {
	root := flag.String("root", "..", "repository root")
	out := flag.String("out", "../_site", "output directory")
	siteURL := flag.String("url", "https://rossedman.io/postmarkd", "public URL of the site, for social cards and canonical links")
	flag.Parse()
	log.SetFlags(0)

	s, err := build(*root, strings.TrimSuffix(*siteURL, "/"))
	if err != nil {
		log.Fatal(err)
	}
	if err := s.write(*out); err != nil {
		log.Fatal(err)
	}
	log.Printf("built %d pages into %s", len(s.pages), *out)
}

type site struct {
	root, url string
	sections  []Section
	pages     []*Page
	bySource  map[string]*Page // repo-relative source path -> page
	tmpl      *template.Template
	year      int
	mark      template.HTML // inline logo mark, colored by site CSS
	version   string        // latest release, from CHANGELOG.md
}

func build(root, url string) (*site, error) {
	s := &site{root: root, url: url, bySource: map[string]*Page{}, year: time.Now().Year()}
	for _, sec := range nav {
		section := Section{Title: sec.title}
		for _, sp := range sec.pages {
			p := &Page{Slug: sp[0], Source: sp[1], Section: sec.title}
			section.Pages = append(section.Pages, p)
			s.pages = append(s.pages, p)
			s.bySource[p.Source] = p
		}
		s.sections = append(s.sections, section)
	}
	for i, p := range s.pages {
		if i > 0 {
			p.Prev = s.pages[i-1]
		}
		if i < len(s.pages)-1 {
			p.Next = s.pages[i+1]
		}
	}

	// Pass 1: parse every page so anchors are known; pass 2: render with
	// rewritten, validated links.
	for _, p := range s.pages {
		src, err := os.ReadFile(filepath.Join(root, p.Source))
		if err != nil {
			return nil, err
		}
		p.parse(src)
	}
	var problems []string
	for _, p := range s.pages {
		problems = append(problems, p.render(s)...)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("broken links:\n  %s", strings.Join(problems, "\n  "))
	}

	var err error
	if s.version, err = latestVersion(root); err != nil {
		return nil, err
	}
	if stale := staleVersions(root, s.version); len(stale) > 0 {
		return nil, fmt.Errorf("install instructions don't match the latest release %s (CHANGELOG.md):\n  %s",
			s.version, strings.Join(stale, "\n  "))
	}

	markSVG, err := os.ReadFile(filepath.Join(root, "assets", "brand", "mark.svg"))
	if err != nil {
		return nil, err
	}
	inner := regexp.MustCompile(`(?s)<svg[^>]*>.*?</style>(.*)</svg>`).FindSubmatch(markSVG)
	if inner == nil {
		return nil, fmt.Errorf("unexpected mark.svg format")
	}
	s.mark = template.HTML(`<svg class="mark" viewBox="0 0 96 96" aria-hidden="true">` + string(inner[1]) + `</svg>`)

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"year": func() int { return s.year },
	}).ParseFS(embedded, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = tmpl
	return s, nil
}

// pageData is what templates receive.
type pageData struct {
	Root        string // relative path to the site root, "" or "../"
	URL         string // public site URL
	Path        string // this page's path from the root
	Title       string
	Description string
	Page        *Page
	Sections    []Section
	Repo        string
	Mark        template.HTML
	Version     string // latest release, e.g. "0.1.0"
	Minor       string // its image tag, e.g. "0.1"
}

func (s *site) write(out string) error {
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	render := func(name, file string, d pageData) error {
		var buf bytes.Buffer
		if err := s.tmpl.ExecuteTemplate(&buf, name, d); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		dst := filepath.Join(out, file)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, buf.Bytes(), 0o644)
	}
	base := pageData{URL: s.url, Sections: s.sections, Repo: repoURL, Mark: s.mark,
		Version: s.version, Minor: minor(s.version)}

	home := base
	home.Path, home.Title = "", "postmarkd: send mail from anything, through Postmark"
	home.Description = "A small, fast daemon that lets every app on a host or in a pod send mail through Postmark over HTTP, SMTP, sendmail or a CLI. Single static Go binary, systemd native, container ready."
	if err := render("page-index", "index.html", home); err != nil {
		return err
	}
	nf := base
	nf.Root, nf.Title = s.url+"/", "Page not found · postmarkd"
	if err := render("page-404", "404.html", nf); err != nil {
		return err
	}
	for _, p := range s.pages {
		d := base
		d.Root, d.Path, d.Page = "../", "docs/"+p.Slug+".html", p
		d.Title, d.Description = p.Title+" · postmarkd", p.Description
		if err := render("page-doc", d.Path, d); err != nil {
			return err
		}
	}

	// Static files and brand assets.
	if err := copyFS(embedded, "static", out); err != nil {
		return err
	}
	brand := os.DirFS(filepath.Join(s.root, "assets", "brand"))
	for _, f := range []string{"mark.svg", "logo.svg", "logo-light.svg", "logo-dark.svg", "favicon.svg", "social-card.png", "banner-light.png", "banner-dark.png"} {
		b, err := fs.ReadFile(brand, f)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "assets", f), b, 0o644); err != nil {
			return err
		}
	}

	// Images referenced by the docs.
	imgs, _ := filepath.Glob(filepath.Join(s.root, "docs", "images", "*"))
	if len(imgs) > 0 {
		if err := os.MkdirAll(filepath.Join(out, "docs", "images"), 0o755); err != nil {
			return err
		}
	}
	for _, f := range imgs {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "docs", "images", filepath.Base(f)), b, 0o644); err != nil {
			return err
		}
	}

	// Search index.
	var idx []searchEntry
	for _, p := range s.pages {
		idx = append(idx, p.search...)
	}
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "assets", "search.json"), b, 0o644); err != nil {
		return err
	}

	// Sitemap.
	var sm strings.Builder
	sm.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	fmt.Fprintf(&sm, "  <url><loc>%s/</loc></url>\n", s.url)
	for _, p := range s.pages {
		fmt.Fprintf(&sm, "  <url><loc>%s/docs/%s.html</loc></url>\n", s.url, p.Slug)
	}
	sm.WriteString("</urlset>\n")
	if err := os.WriteFile(filepath.Join(out, "sitemap.xml"), []byte(sm.String()), 0o644); err != nil {
		return err
	}
	// Serve files as-is (no Jekyll processing on GitHub Pages).
	return os.WriteFile(filepath.Join(out, ".nojekyll"), nil, 0o644)
}

func copyFS(fsys fs.FS, dir, out string) error {
	return fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(out, "assets", strings.TrimPrefix(p, dir))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

// resolveLink maps a link found in source file from to its URL in the site.
// It returns ok=false with a reason if the target does not exist.
func (s *site) resolveLink(from *Page, dest string) (string, string) {
	if dest == "" || strings.Contains(dest, "://") || strings.HasPrefix(dest, "mailto:") {
		return dest, ""
	}
	target, frag, _ := strings.Cut(dest, "#")
	if target == "" { // same-page anchor
		if frag != "" && !from.ids[frag] {
			return dest, fmt.Sprintf("%s: no heading #%s", from.Source, frag)
		}
		return dest, ""
	}
	repoPath := path.Clean(path.Join(path.Dir(from.Source), target))
	if p, ok := s.bySource[repoPath]; ok {
		if frag != "" && !p.ids[frag] {
			return dest, fmt.Sprintf("%s: %s has no heading #%s", from.Source, repoPath, frag)
		}
		u := p.Slug + ".html"
		if frag != "" {
			u += "#" + frag
		}
		return u, ""
	}
	if repoPath == "README.md" {
		return "../index.html", ""
	}
	// Images under docs/images are copied next to the pages.
	if rel, ok := strings.CutPrefix(repoPath, "docs/images/"); ok {
		if _, err := os.Stat(filepath.Join(s.root, "docs", "images", rel)); err != nil {
			return dest, fmt.Sprintf("%s: missing image %s", from.Source, target)
		}
		return "images/" + rel, ""
	}
	// Any other repository file or directory: link to it on GitHub.
	fi, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(repoPath)))
	if err != nil || strings.HasPrefix(repoPath, "..") {
		return dest, fmt.Sprintf("%s: link to missing file %s", from.Source, target)
	}
	kind := "blob"
	if fi.IsDir() {
		kind = "tree"
	}
	u := repoURL + "/" + kind + "/master/" + repoPath
	if frag != "" {
		u += "#" + frag
	}
	return u, ""
}

var tableRE = regexp.MustCompile(`(?s)<table>.*?</table>`)

var (
	releaseRE    = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`)
	installVerRE = regexp.MustCompile(`VERSION=(\d+\.\d+\.\d+)`)
	imageTagRE   = regexp.MustCompile(`ghcr\.io/rossedman/postmarkd:(\d+\.\d+(?:\.\d+)?)`)
)

// latestVersion is the newest released version in CHANGELOG.md.
func latestVersion(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		return "", err
	}
	m := releaseRE.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("CHANGELOG.md has no released version")
	}
	return string(m[1]), nil
}

func minor(v string) string {
	parts := strings.SplitN(v, ".", 3)
	return parts[0] + "." + parts[1]
}

// staleVersions finds install commands and image tags in the docs, README,
// examples and site templates that point at an older release.
func staleVersions(root, version string) []string {
	var files []string
	for _, pattern := range []string{"README.md", "docs/*.md", "examples/*/*", "site/templates/*.html"} {
		m, _ := filepath.Glob(filepath.Join(root, pattern))
		files = append(files, m...)
	}
	var stale []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, f)
		for _, m := range installVerRE.FindAllSubmatch(b, -1) {
			if string(m[1]) != version {
				stale = append(stale, fmt.Sprintf("%s: VERSION=%s", rel, m[1]))
			}
		}
		for _, m := range imageTagRE.FindAllSubmatch(b, -1) {
			if t := string(m[1]); t != version && t != minor(version) {
				stale = append(stale, fmt.Sprintf("%s: image tag %s", rel, t))
			}
		}
	}
	return stale
}
