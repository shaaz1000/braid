// Package probe asks a URL three questions before any bonded transfer starts:
// how big is it, can it be split, and has it changed since last time.
package probe

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Result describes what a URL will let us do.
type Result struct {
	// URL is the final URL after redirects. Chunk workers request this
	// directly rather than re-following redirects on every chunk.
	URL string
	// Size is the total length of the file in bytes, always known.
	Size int64
	// Ranges reports whether the server answered a ranged request with 206.
	// When false the transfer cannot be split and must run on one link.
	Ranges bool
	// ETag and LastModified are the validators a resume is checked against.
	ETag         string
	LastModified string
	// Filename is a safe base name with no directory component.
	Filename string
}

// Do probes rawURL with a one-byte ranged GET.
//
// A ranged GET is used rather than a HEAD because servers and CDNs frequently
// misreport HEAD, whereas a 206 response is conclusive proof that byte ranges
// work. A 200 answer means the server ignored the Range header: that is not an
// error, it just means the file cannot be split.
func Do(ctx context.Context, client *http.Client, rawURL string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// resp.Request.URL reflects the last hop the client actually followed.
	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	out := Result{
		URL:          finalURL,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Filename:     filename(resp.Header.Get("Content-Disposition"), finalURL),
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		size, err := totalFromContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return Result{}, err
		}
		out.Ranges, out.Size = true, size

	case http.StatusOK:
		if resp.ContentLength < 0 {
			return Result{}, fmt.Errorf("server answered 200 with no Content-Length; size unknown")
		}
		out.Ranges, out.Size = false, resp.ContentLength

	default:
		return Result{}, fmt.Errorf("probe got HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	if out.Size <= 0 {
		return Result{}, fmt.Errorf("file is empty (size %d)", out.Size)
	}
	return out, nil
}

// totalFromContentRange reads the total length out of a header shaped like
// "bytes 0-0/1048576". A total of "*" means the server will not say, which
// leaves nothing to divide into chunks.
func totalFromContentRange(v string) (int64, error) {
	if v == "" {
		return 0, fmt.Errorf("206 response with no Content-Range header")
	}
	unit, spec, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok || unit != "bytes" {
		return 0, fmt.Errorf("unsupported Content-Range %q", v)
	}
	_, total, ok := strings.Cut(spec, "/")
	if !ok {
		return 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	total = strings.TrimSpace(total)
	if total == "*" {
		return 0, fmt.Errorf("server will not report total size in Content-Range %q", v)
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	return n, nil
}

// filename picks a safe base name, preferring Content-Disposition over the URL
// path. Any directory component is discarded: a server must not be able to
// choose where on disk we write by naming a file "../../etc/passwd".
func filename(disposition, finalURL string) string {
	if disposition != "" {
		if _, params, err := mime.ParseMediaType(disposition); err == nil {
			if name := safeBase(params["filename"]); name != "" {
				return name
			}
		}
	}
	if u, err := url.Parse(finalURL); err == nil {
		if name := safeBase(u.Path); name != "" {
			return name
		}
	}
	return "download"
}

// safeBase reduces a candidate to a bare filename, or "" if nothing usable
// remains.
func safeBase(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	// Normalise Windows-style separators before taking the base, so a name
	// like `..\..\evil` cannot survive.
	candidate = strings.ReplaceAll(candidate, `\`, "/")
	base := path.Base(path.Clean(candidate))
	switch base {
	case ".", "..", "/":
		return ""
	}
	return base
}
