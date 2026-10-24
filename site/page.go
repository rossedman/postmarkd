package main

import (
	"bytes"
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"unicode"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Page is one documentation page.
type Page struct {
	Slug, Source, Section string
	Title, Description    string
	NavTitle              string // shorter title for the sidebar
	TOC                   []TOCItem
	Content               string // rendered HTML; parsed as a template so html/template checks it
	Prev, Next            *Page

	src    []byte
	doc    ast.Node
	ids    map[string]bool
	search []searchEntry
}

// TOCItem is an entry of the "On this page" outline.
type TOCItem struct {
	ID, Text string
	Level    int
}

type searchEntry struct {
	Title   string `json:"t"`           // page title
	Heading string `json:"h,omitempty"` // section heading
	URL     string `json:"u"`           // relative to the site root
	Text    string `json:"x"`           // plain text excerpt
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(
		html.WithUnsafe(),
		renderer.WithNodeRenderers(util.Prioritized(codeRenderer{}, 100)),
	),
)

func (p *Page) parse(src []byte) {
	p.src = src
	ids := &headingIDs{seen: map[string]int{}}
	p.doc = md.Parser().Parse(text.NewReader(src), parser.WithContext(parser.NewContext(parser.WithIDs(ids))))
	p.ids = map[string]bool{}

	var section *searchEntry
	var excerpt strings.Builder
	flush := func() {
		if section != nil {
			section.Text = clip(strings.Join(strings.Fields(excerpt.String()), " "), 600)
			p.search = append(p.search, *section)
		}
		excerpt.Reset()
	}
	url := "docs/" + p.Slug + ".html"

	for n := p.doc.FirstChild(); n != nil; {
		next := n.NextSibling()
		switch n := n.(type) {
		case *ast.Heading:
			id := attr(n, "id")
			p.ids[id] = true
			t := plain(n, src)
			switch {
			case n.Level == 1 && p.Title == "":
				p.Title = t
				p.doc.RemoveChild(p.doc, n) // the template renders the title
				section = &searchEntry{Title: t, URL: url}
			case n.Level <= 3:
				p.TOC = append(p.TOC, TOCItem{ID: id, Text: t, Level: n.Level})
				if n.Level == 2 {
					flush()
					section = &searchEntry{Title: p.Title, Heading: t, URL: url + "#" + id}
				}
			}
		case *ast.Paragraph:
			if p.Description == "" && p.Title != "" {
				p.Description = clip(strings.Join(strings.Fields(plain(n, src)), " "), 200)
			}
			excerpt.WriteString(plain(n, src) + " ")
		case *ast.List, *ast.Blockquote:
			excerpt.WriteString(plain(n, src) + " ")
		}
		// Also index deeper headings so they are linkable.
		ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
			if h, ok := c.(*ast.Heading); ok && entering {
				p.ids[attr(h, "id")] = true
			}
			return ast.WalkContinue, nil
		})
		n = next
	}
	flush()
	if p.Title == "" {
		p.Title = p.Slug
	}
	p.NavTitle = p.Title
	if t, ok := navTitles[p.Slug]; ok {
		p.NavTitle = t
	}
}

var headingTagRE = regexp.MustCompile(`<h([23]) id="([^"]+)">`)

// render rewrites links and renders HTML. It returns link problems.
func (p *Page) render(s *site) []string {
	var problems []string
	ast.Walk(p.doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link:
			u, problem := s.resolveLink(p, string(n.Destination))
			n.Destination = []byte(u)
			if problem != "" {
				problems = append(problems, problem)
			}
			if strings.HasPrefix(u, "http") {
				n.SetAttributeString("rel", []byte("noopener"))
			}
		case *ast.Image:
			u, problem := s.resolveLink(p, string(n.Destination))
			n.Destination = []byte(u)
			if problem != "" {
				problems = append(problems, problem)
			}
		}
		return ast.WalkContinue, nil
	})

	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, p.src, p.doc); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", p.Source, err))
	}
	out := tableRE.ReplaceAllStringFunc(buf.String(), func(t string) string {
		return `<div class="table-wrap">` + t + `</div>`
	})
	out = headingTagRE.ReplaceAllString(out, `<h$1 id="$2"><a class="anchor" href="#$2" aria-label="Link to this section">#</a>`)
	p.Content = out
	return problems
}

// headingIDs generates GitHub-compatible heading IDs, so anchors written
// for GitHub's Markdown rendering work on the site too.
type headingIDs struct{ seen map[string]int }

func (h *headingIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	var b strings.Builder
	for _, r := range strings.ToLower(string(value)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	id := b.String()
	if n := h.seen[id]; n > 0 {
		h.seen[id] = n + 1
		id = fmt.Sprintf("%s-%d", id, n)
	} else {
		h.seen[id] = 1
	}
	return []byte(id)
}

func (h *headingIDs) Put(value []byte) { h.seen[string(value)]++ }

func attr(n ast.Node, name string) string {
	v, ok := n.AttributeString(name)
	if !ok {
		return ""
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

// plain returns the text content of a node.
func plain(n ast.Node, src []byte) string {
	var b strings.Builder
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			if c.Type() == ast.TypeBlock {
				b.WriteByte(' ') // separate paragraphs and list items
			}
			return ast.WalkContinue, nil
		}
		switch c := c.(type) {
		case *ast.Text:
			b.Write(c.Segment.Value(src))
			if c.SoftLineBreak() || c.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(c.Value)
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	if i := strings.LastIndexByte(s, ' '); i > n/2 {
		s = s[:i]
	}
	return s + "…"
}

// codeRenderer renders code blocks with syntax highlighting, a language
// label and a copy button.
type codeRenderer struct{}

func (codeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, renderCode)
	reg.Register(ast.KindCodeBlock, renderCode)
}

var formatter = chromahtml.New(chromahtml.WithClasses(true), chromahtml.PreventSurroundingPre(true))

var langNames = map[string]string{
	"sh": "shell", "bash": "shell", "console": "shell", "ini": "config", "yaml": "yaml",
	"json": "json", "go": "go", "js": "javascript", "ruby": "ruby", "python": "python",
	"dockerfile": "dockerfile", "http": "http", "text": "", "": "",
}

func renderCode(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	var lang string
	if f, ok := n.(*ast.FencedCodeBlock); ok && f.Info != nil {
		lang = strings.Fields(string(f.Info.Segment.Value(src)) + " ")[0]
	}
	var code strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		code.Write(seg.Value(src))
	}

	lexer := lexers.Get(lang)
	if lexer == nil || lang == "" || lang == "text" {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	it, err := lexer.Tokenise(nil, code.String())
	if err != nil {
		return ast.WalkStop, err
	}
	label, ok := langNames[lang]
	if !ok {
		label = lang
	}
	fmt.Fprintf(w, `<div class="code" data-lang="%s"><div class="code-bar"><span class="code-lang">%s</span><button class="copy" type="button" aria-label="Copy code">Copy</button></div><pre class="chroma"><code>`,
		template.HTMLEscapeString(lang), template.HTMLEscapeString(label))
	if err := formatter.Format(w, styles.Fallback, it); err != nil {
		return ast.WalkStop, err
	}
	w.WriteString("</code></pre></div>\n")
	return ast.WalkSkipChildren, nil
}
