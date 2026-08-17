package checker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	awsSigV4Algorithm   = "AWS4-HMAC-SHA256"
	awsSigV4Service     = "s3"
	unsignedPayloadHash = "UNSIGNED-PAYLOAD"
	emptyPayloadSHA256  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	amzDateFormat       = "20060102T150405Z"
	amzDateStampFormat  = "20060102"
)

// signS3Request attaches SigV4 headers to req. payloadHash is typically
// UNSIGNED-PAYLOAD for GET/HEAD health probes.
func signS3Request(req *http.Request, opts s3Opts, now time.Time, payloadHash string) {
	now = now.UTC()
	if payloadHash == "" {
		payloadHash = unsignedPayloadHash
	}
	amzDate := now.Format(amzDateFormat)
	dateStamp := now.Format(amzDateStampFormat)

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if opts.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", opts.SessionToken)
	}

	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	req.Header.Set("Host", host)

	canonicalURI := s3CanonicalURI(req)
	canonicalQuery := s3CanonicalQuery(req)
	signedHeaders, canonicalHeaders := s3CanonicalHeaders(req)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{dateStamp, opts.Region, awsSigV4Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		awsSigV4Algorithm,
		amzDate,
		credentialScope,
		hexSHA256(canonicalRequest),
	}, "\n")

	signingKey := s3SigningKey(opts.SecretKey, dateStamp, opts.Region)
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	req.Header.Set("Authorization", awsSigV4Algorithm+
		" Credential="+opts.AccessKey+"/"+credentialScope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
}

func s3CanonicalURI(req *http.Request) string {
	if req.URL == nil {
		return "/"
	}
	if req.URL.Opaque != "" {
		// Not used by this checker; keep a safe default.
		return "/"
	}
	path := req.URL.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

func s3CanonicalQuery(req *http.Request) string {
	if req.URL == nil || req.URL.RawQuery == "" {
		return ""
	}
	values := req.URL.Query()
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range values[k] {
			parts = append(parts, s3URIEncode(k, true)+"="+s3URIEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func s3CanonicalHeaders(req *http.Request) (signedHeaders, canonicalHeaders string) {
	type kv struct{ name, value string }
	var headers []kv
	seen := map[string]bool{}

	add := func(name, value string) {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || name == "authorization" {
			return
		}
		if seen[name] {
			return
		}
		seen[name] = true
		headers = append(headers, kv{name: name, value: collapseSpaces(value)})
	}

	if req.Host != "" {
		add("host", req.Host)
	} else if req.URL != nil {
		add("host", req.URL.Host)
	}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if lower != "host" && lower != "x-amz-date" && lower != "x-amz-content-sha256" && lower != "x-amz-security-token" && lower != "range" && lower != "date" {
			continue
		}
		add(name, strings.Join(values, ","))
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].name < headers[j].name })

	names := make([]string, len(headers))
	var b strings.Builder
	for i, h := range headers {
		names[i] = h.name
		b.WriteString(h.name)
		b.WriteByte(':')
		b.WriteString(h.value)
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

func s3SigningKey(secret, dateStamp, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, awsSigV4Service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hexSHA256(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// s3URIEncode implements the AWS SigV4 URI encoding rules: unreserved
// characters stay literal; every other byte becomes %XX. Slash is preserved
// when encodeSlash is false.
func s3URIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else if c == '/' && !encodeSlash {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}

const upperHex = "0123456789ABCDEF"

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
