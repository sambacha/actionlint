package main

import (
	"bytes"
	"fmt"
	"go/format"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var dbg = log.New(io.Discard, "", log.LstdFlags)
var reReplaceholder = regexp.MustCompile("{%[^%]+%}")

func parseContextAvailabilityTable(src []byte) (*extast.Table, bool) {
	md := goldmark.New(goldmark.WithExtensions(extension.Table))
	root := md.Parser().Parse(text.NewReader(src))
	n := root.FirstChild()

	for ; n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok && h.Level == 3 && bytes.Equal(h.Text(src), []byte("Context availability")) {
			n = n.NextSibling()
			break
		}
	}

	for ; n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok && h.Level == 3 {
			return nil, false
		}
		if t, ok := n.(*extast.Table); ok {
			return t, true
		}
	}

	return nil, false
}

func cells(n *extast.TableRow, src []byte) []string {
	t := []string{}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if tc, ok := c.(*extast.TableCell); ok {
			t = append(t, string(tc.Text(src)))
		}
	}
	return t
}

func split(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{}
	}

	ss := strings.Split(text, ",")
	for i, s := range ss {
		ss[i] = strings.ToLower(strings.TrimSpace(s))
	}
	sort.Strings(ss)
	return ss
}

func stripAndUnescape(s string) (string, error) {
	if strings.Contains(s, "{% else %}") {
		return "", fmt.Errorf("cannot strip template directives since it contains {%% else %%}: %s", s)
	}
	s = reReplaceholder.ReplaceAllString(s, "")
	return html.UnescapeString(s), nil
}

func generate(src []byte, out io.Writer) error {
	t, ok := parseContextAvailabilityTable(src)
	if !ok {
		return fmt.Errorf("no \"Context availability\" table was found")
	}
	dbg.Println("\"Context availability\" table was found")

	funcs := map[string][]string{}
	buf := &bytes.Buffer{}

	fmt.Fprintln(buf, `// Code generated by actionlint/scripts/generate-context-availability. DO NOT EDIT.

package actionlint

// ContextAvailability returns 2 values availability of given workflow key.
// 1st return value indicates what contexts are available. Empty slice means any contexts are available.
// 2nd return value indicates what special functions are available. Empty slice means no special functions are available.
// The 'key' parameter should represents a workflow key like "jobs.<job_id>.concurrency".
//
// This function was generated from https://docs.github.com/en/actions/learn-github-actions/contexts#context-availability.
// See the script for more details: https://github.com/rhysd/actionlint/blob/main/scripts/generate-context-availability/
func ContextAvailability(key string) ([]string, []string) {
	switch key {`)

	keys := []string{}
	for n := t.FirstChild(); n != nil; n = n.NextSibling() {
		r, ok := n.(*extast.TableRow)
		if !ok {
			continue
		}
		cs := cells(r, src)
		if len(cs) != 3 {
			return fmt.Errorf("expected 3 rows in table but got %v", cs)
		}
		if cs[0] == "{% else %}" {
			dbg.Println("Found {% else %} directive. Breaking from loop of rows")
			break
		}

		for i, c := range cs {
			c, err := stripAndUnescape(c)
			if err != nil {
				return err
			}
			cs[i] = c
		}

		key := cs[0]
		if key == "" {
			dbg.Printf("Skip empty key at %q\n", r.Text(src))
			continue
		}
		ctx := split(cs[1])
		sp := split(cs[2])

		for _, s := range sp {
			funcs[s] = append(funcs[s], key)
		}

		fmt.Fprintf(buf, "	case %q: return %#v, %#v\n", key, ctx, sp)
		dbg.Println("Parsed table row:", key)
		keys = append(keys, key)
	}
	fmt.Fprintln(buf, "	default: return nil, nil\n	}\n}")
	dbg.Println("Parsed", len(keys), "table rows")

	fmt.Fprintln(buf, `// SpecialFunctionNames is a map from special function name to available workflow keys.
// Some functions are only available at specific positions. This variable is useful when you want to
// know which functions are special and what workflow keys support them.
//
// This function was generated from https://docs.github.com/en/actions/learn-github-actions/contexts#context-availability.
// See the script for more details: https://github.com/rhysd/actionlint/blob/main/scripts/generate-context-availability/`)
	fmt.Fprintf(buf, "var SpecialFunctionNames = %#v\n", funcs)

	// This variabel is for unit tests
	sort.Strings(keys)
	fmt.Fprintf(buf, "\nvar allWorkflowKeys = %#v\n", keys)

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("could not format Go source: %w", err)
	}

	if _, err := out.Write(formatted); err != nil {
		return fmt.Errorf("could not write output: %w", err)
	}

	return nil
}

func source(args []string, url string) ([]byte, error) {
	if len(args) == 2 {
		return os.ReadFile(args[0])
	}

	var c http.Client

	dbg.Println("Fetching source from URL:", url)

	res, err := c.Get(url)
	if err != nil {
		return nil, fmt.Errorf("could not fetch %s: %w", url, err)
	}
	if res.StatusCode < 200 || 300 <= res.StatusCode {
		return nil, fmt.Errorf("request was not successful for %s: %s", url, res.Status)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("could not fetch body for %s: %w", url, err)
	}
	res.Body.Close()

	dbg.Printf("Fetched %d bytes from %s", len(body), url)
	return body, nil
}

func run(args []string, stdout, stderr, dbgout io.Writer, srcURL string) int {
	dbg.SetOutput(dbgout)

	if len(args) > 2 {
		fmt.Fprintln(stderr, "usage: generate-context-availability [[srcfile] dstfile]")
		return 1
	}

	dbg.Println("Start generate-context-availability")

	src, err := source(args, srcURL)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	out := stdout
	dst := "<stdout>"
	if len(args) > 0 && args[len(args)-1] != "-" {
		dst = args[len(args)-1]
		f, err := os.Create(dst)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer f.Close()
		out = f
	}

	dbg.Println("Writing output to", dst)

	if err := generate(src, out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	dbg.Println("Wrote output to", dst)
	dbg.Println("Done generate-context-availability script successfully")
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Stderr, "https://raw.githubusercontent.com/github/docs/main/content/actions/learn-github-actions/contexts.md"))
}
