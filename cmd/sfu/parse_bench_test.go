package main

import (
	"testing"
)

// bench pools, synthetic so CI reproducible. covers host:user:pw,
// host:port:user:pw, bare LPU, email/unicode/token variants
type corpora struct {
	ulpValid []string
	lpuValid []string
	garbage  []string
}

var benchCorpora corpora

func init() {
	benchCorpora.ulpValid = []string{
		"https://example.com:user@example.com:secret123",
		"http://www.test.com/login:alice:Password!",
		"foo.bar.com:8080/path?q=1:bob:tok",
		"https://accounts.google.com/signin:user12@gmail.com:hunter2",
		"https://login.microsoftonline.com:jane.doe:MyP@ssw0rd",
		"https://www.facebook.com:fbuser:fbpass",
		"https://www.instagram.com:igname:igpw",
		"https://twitter.com:tw_user:tweetpw",
		"https://github.com/login:gh-user:gh-secret",
		"https://api.service.io/v2/login:apiuser:apitoken",
	}
	benchCorpora.garbage = []string{
		"____________________________________________________________________________________________________________________________________________",
		"????????????????????????????????????????",
		"no-colons-here-at-all-line",
		"only:one:colon",
		"localhost:bob:pw",
		"127.0.0.1:alice:pw",
		"https://example.com:{user}:secret",
		"https://example.com:user:" + repeatChar('x', 100),
		"random ascii line with no structure whatsoever just words",
		"::::::",
	}
	// LPU pool mirrors real reject shapes: host:user:pw, host:port:user:pw,
	// bare TLD variants, unicode + email logins. inline so no testdata ship
	benchCorpora.lpuValid = []string{
		"shop.example.com:alice:p@ssw0rd",
		"mail.example.org:bob.smith:hunter2",
		"login.bank.io:carol:S3cret!",
		"api.svc.local:8443:dave:tokenABC",
		"ldap.corp.net:389:admin:N4VyB3an",
		"smtp.gmail.com:587:user@gmail.com:appspecific",
		"db.cluster:5432:postgres:longerpassword",
		"vpn.acme.co:1194:engineer:VpnPass!2024",
		"ssh.box.dev:2222:root:rootpw",
		"www.shop.example.com:eve@shop.example.com:checkout",
		"my.bank.fr:gabrielle:café-au-lait",
		"intranet:joe:joepass",
		"hostname-only:user:p",
		"sub.domain.tld:user_with_underscore:tokenAAA",
		"123.45.67.89:ipuser:ippass",
		"crm.x.io:carlos.lopez@x.io:Migración#1",
		"checkout.example.com:8080:cust:pinCode4242",
		"helpdesk.example.com:ticket-bot:auto-pass",
		"remote.lab.net:22:operator:opspw",
		"support.example.com:agent:chatpw",
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

// 100% ULP-valid (~95% happy path). loose runs strict first so should match
func BenchmarkParse_ULPHappy(b *testing.B) {
	pool := benchCorpora.ulpValid
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parse(pool[i%len(pool)])
	}
}

func BenchmarkParseLoose_ULPHappy(b *testing.B) {
	pool := benchCorpora.ulpValid
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseLoose(pool[i%len(pool)])
	}
}

// 100% malformed. junk filter catches some early, rest pays regex +
// matchLPU + splitNColon
func BenchmarkParse_Garbage(b *testing.B) {
	pool := benchCorpora.garbage
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parse(pool[i%len(pool)])
	}
}

func BenchmarkParseLoose_Garbage(b *testing.B) {
	pool := benchCorpora.garbage
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseLoose(pool[i%len(pool)])
	}
}

// LPU shapes, both modes go through matchLPU. measures the LPU branch
func BenchmarkParse_LPU(b *testing.B) {
	pool := benchCorpora.lpuValid
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parse(pool[i%len(pool)])
	}
}

func BenchmarkParseLoose_LPU(b *testing.B) {
	pool := benchCorpora.lpuValid
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseLoose(pool[i%len(pool)])
	}
}

// realistic 95% ULP / 3% LPU / 2% garbage, tracks prod throughput
func BenchmarkParse_Mixed(b *testing.B) {
	pool := buildMixedPool()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parse(pool[i%len(pool)])
	}
}

func BenchmarkParseLoose_Mixed(b *testing.B) {
	pool := buildMixedPool()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseLoose(pool[i%len(pool)])
	}
}

// per-line cost after parse(): format + hash. A/B vs lineFormatter
// (reused bytes.Buffer + streaming xxhash.Digest)
func BenchmarkFormatRecord_StringPath(b *testing.B) {
	const host = "accounts.google.com"
	const url = "https://accounts.google.com/signin"
	const login = "user12@gmail.com"
	const password = "hunter2"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = formatRecord(host, url, login, password, false)
		_ = dedupKey(host, login, password)
	}
}

func BenchmarkFormatRecord_LineFormatter(b *testing.B) {
	const host = "accounts.google.com"
	const url = "https://accounts.google.com/signin"
	const login = "user12@gmail.com"
	const password = "hunter2"
	lf := newLineFormatter()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = lf.FormatRecord(host, url, login, password, false)
		_ = lf.HashKey(host, login, password)
	}
}

func buildMixedPool() []string {
	const total = 10_000
	const ulpShare = 9500 // 95%
	const lpuShare = 300  // 3%
	const junkShare = 200 // 2%

	pool := make([]string, 0, total)
	for i := 0; i < ulpShare; i++ {
		pool = append(pool, benchCorpora.ulpValid[i%len(benchCorpora.ulpValid)])
	}
	if n := len(benchCorpora.lpuValid); n > 0 {
		for i := 0; i < lpuShare; i++ {
			pool = append(pool, benchCorpora.lpuValid[i%n])
		}
	}
	for i := 0; i < junkShare; i++ {
		pool = append(pool, benchCorpora.garbage[i%len(benchCorpora.garbage)])
	}
	return pool
}
