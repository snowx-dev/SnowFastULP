package sflog

import (
	"bufio"
	"io"
	"net/url"
	"strings"

	"github.com/snowx-dev/SnowFastULP/internal/textenc"
)

type credBlock struct {
	url      string
	username string
	password string
}

func ParseCredentials(r io.Reader, source string) ([]Credential, error) {
	// Stealer logs are routinely UTF-16LE-BOM (RedLine/Vidar) or UTF-8-BOM;
	// decode to UTF-8 first so a Windows-origin Passwords.txt isn't parsed as
	// garbage (silent zero creds) or stripped of its first record.
	sc := bufio.NewScanner(textenc.WrapReader(r))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var out []Credential
	var block credBlock
	flush := func() {
		if block.url != "" && block.username != "" && block.password != "" {
			out = append(out, Credential{
				URL: block.url, Username: block.username, Password: block.password, Source: source,
			})
		}
		block = credBlock{}
	}

	for sc.Scan() {
		raw := strings.TrimRight(sc.Text(), "\r\n")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || isSeparator(trimmed) {
			flush()
			continue
		}
		key, val, ok := splitField(raw)
		if !ok || val == "" {
			continue
		}
		switch classifyField(key) {
		case "url":
			// A second url-class field re-appearing signals a new record only
			// when the current one already has a user+password (url-first
			// layout). On an incomplete block it is a duplicate alias within
			// the same record (e.g. URL + Host), so keep the first.
			if block.url != "" {
				if block.username != "" && block.password != "" {
					flush()
					block.url = val
				}
			} else {
				block.url = val
			}
		case "username":
			if block.username != "" {
				flush()
			}
			block.username = val
		case "password":
			if block.password != "" {
				flush()
			}
			block.password = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}

// splitField parses "Key: value". The key is fully trimmed; the value keeps its
// meaningful whitespace, dropping only the single conventional delimiter space
// after the colon so passwords with leading/trailing spaces survive.
func splitField(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(line[:i]))
	val = strings.TrimPrefix(line[i+1:], " ")
	return key, val, true
}

func classifyField(key string) string {
	key = strings.Join(strings.Fields(key), " ")
	switch key {
	case "url", "ur1", "host", "hostname":
		return "url"
	case "user", "login", "username", "user login", "u53rn4m3":
		return "username"
	case "pass", "password", "user password", "p455w0rd":
		return "password"
	default:
		return ""
	}
}

func isSeparator(line string) bool {
	if len(line) < 3 {
		return false
	}
	for _, r := range line {
		if r != '=' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func FormatULPLine(c Credential, noURI bool) string {
	urlPart := normalizeURL(strings.TrimSpace(c.URL))
	host := hostFromURL(urlPart)
	if noURI {
		urlPart = host
	}
	var b strings.Builder
	b.Grow(len(urlPart) + len(c.Username) + len(c.Password) + 2)
	b.WriteString(urlPart)
	b.WriteByte(':')
	b.WriteString(c.Username)
	b.WriteByte(':')
	b.WriteString(c.Password)
	return b.String()
}

// normalizeURL drops the http(s) scheme so web URLs match the sfu line shape.
// Non-web schemes (android://, etc.) are kept verbatim, exactly like sfu's
// stripScheme, so the two pipelines emit identical lines for the same input.
func normalizeURL(s string) string {
	return stripWebScheme(s)
}

func stripWebScheme(s string) string {
	if len(s) >= 7 && strings.EqualFold(s[:7], "http://") {
		return s[7:]
	}
	if len(s) >= 8 && strings.EqualFold(s[:8], "https://") {
		return s[8:]
	}
	return s
}

func hostFromURL(s string) string {
	raw := s
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		return strings.TrimPrefix(u.Host, "www.")
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "www.")
}
