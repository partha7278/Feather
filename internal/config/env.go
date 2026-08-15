package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Provider says which API a destination speaks. The distinction between S3 and
// OCI matters even though both are S3: non-AWS implementations reject the
// checksum headers the AWS SDK now sends by default. Google Drive is a
// different API altogether — folders instead of buckets, OAuth instead of
// signed requests — so it has its own client.
type Provider string

const (
	ProviderS3     Provider = "s3"
	ProviderOCI    Provider = "oci"
	ProviderGDrive Provider = "gdrive"
)

// IsS3 reports whether this provider is reached through the S3 API.
func (p Provider) IsS3() bool { return p == ProviderS3 || p == ProviderOCI }

// Label is the provider's name as the control panel shows it.
func (p Provider) Label() string {
	switch p {
	case ProviderOCI:
		return "Oracle Cloud"
	case ProviderGDrive:
		return "Google Drive"
	default:
		return "AWS S3"
	}
}

// MongoSource is one MongoDB account feather reads from. Several may be
// configured; each job names the one it archives from, so a single daemon can
// serve clusters that share nothing but this process.
type MongoSource struct {
	Alias      string
	URI        string
	AuthSource string
	AppName    string
}

// Destination is one place archives get written — an AWS S3 account, Oracle
// Cloud Object Storage through its S3-compatible endpoint, or a folder in
// Google Drive.
type Destination struct {
	Alias    string
	Provider Provider
	Prefix   string

	// S3 and Oracle.
	Region          string
	Bucket          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	StorageClass    string
	ForcePathStyle  bool

	// Google Drive. Either a service account key or an OAuth client with a
	// refresh token; the folder is where archives are filed, and the folders a
	// key template implies are created inside it.
	DriveFolderID      string
	DriveID            string // the shared drive the folder lives in, if any
	DriveSubject       string // user to impersonate, for domain-wide delegation
	ServiceAccountJSON string
	OAuthClientID      string
	OAuthClientSecret  string
	OAuthRefreshToken  string
}

// DriveRoot is the folder archives go into, defaulting to the account's own My
// Drive when none was named.
func (d Destination) DriveRoot() string {
	if d.DriveFolderID == "" {
		return "root"
	}
	return d.DriveFolderID
}

// Env is what feather needs before it can open its own database. Everything
// else — MongoDB accounts, backup destinations, retention configs, the admin
// login — lives in that database and is managed from the web UI.
type Env struct {
	Environment string
	LogLevel    string
	LogJSON     bool

	DataDir       string
	DBPath        string
	MasterKeyPath string
	MasterKey     string
	SpoolDir      string

	HTTPAddr string
	BaseURL  string

	// TrustedProxies names the networks whose X-Forwarded-For, Forwarded and
	// X-Real-IP headers feather believes. Empty means none: the socket address
	// is the client address, which is the only safe default for a header
	// anybody downstream can write. Accepts CIDRs, bare addresses, and the
	// shorthands "local" and "private".
	TrustedProxies string

	MaxConcurrentJobs int
}

// LoadEnv reads .env (if present) and materialises the process settings. A
// missing .env is not an error — every value has a working default, and a VPS
// may inject real environment variables through systemd instead.
func LoadEnv(envFile string) (*Env, error) {
	if envFile != "" {
		if err := godotenv.Load(envFile); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("loading %s: %w", envFile, err)
		}
	} else {
		_ = godotenv.Load()
	}

	dataDir := getString("FEATHER_DATA_DIR", defaultDataDir())
	env := &Env{
		Environment:       getString("FEATHER_ENV", "production"),
		LogLevel:          getString("LOG_LEVEL", "info"),
		LogJSON:           getBool("LOG_JSON", true),
		DataDir:           dataDir,
		DBPath:            getString("FEATHER_DB", filepath.Join(dataDir, "feather.db")),
		MasterKeyPath:     getString("FEATHER_MASTER_KEY_FILE", filepath.Join(dataDir, "master.key")),
		MasterKey:         getString("FEATHER_MASTER_KEY", ""),
		SpoolDir:          getString("FEATHER_SPOOL_DIR", filepath.Join(dataDir, "spool")),
		HTTPAddr:          getString("FEATHER_HTTP_ADDR", "127.0.0.1:8477"),
		BaseURL:           strings.TrimSuffix(getString("FEATHER_BASE_URL", ""), "/"),
		TrustedProxies:    getString("FEATHER_TRUSTED_PROXIES", ""),
		MaxConcurrentJobs: getInt("FEATHER_MAX_CONCURRENT_JOBS", 2),
	}

	if env.MaxConcurrentJobs < 1 {
		env.MaxConcurrentJobs = 1
	}
	return env, nil
}

// defaultDataDir prefers the system location a service would use, and falls
// back to the working directory when that is not writable — which is what
// happens when someone is trying feather out as their own user.
func defaultDataDir() string {
	const system = "/var/lib/feather"
	if info, err := os.Stat(system); err == nil && info.IsDir() {
		return system
	}
	if err := os.MkdirAll(system, 0o700); err == nil {
		return system
	}
	return "./data"
}

// RedactMongoURI strips the password from a connection string so it can be
// shown in the UI, in logs, and in error messages.
func RedactMongoURI(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		if at := strings.LastIndex(raw, "@"); at >= 0 {
			// Unparseable but clearly carrying credentials — show only the host.
			return "•••@" + raw[at+1:]
		}
		return raw
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		parsed.User = url.UserPassword(parsed.User.Username(), "•••")
	}
	// url.UserPassword re-encodes the placeholder; undo that so it stays legible.
	return strings.ReplaceAll(parsed.String(), "%E2%80%A2%E2%80%A2%E2%80%A2", "•••")
}

// credentialPatterns match the shapes a secret takes when it leaks into an
// error message. Drivers and SDKs are not careful about this: the MongoDB
// driver's URI parse errors quote the URI back verbatim, password and all.
var credentialPatterns = []*regexp.Regexp{
	// Any URL with userinfo — mongodb://, mongodb+srv://, https:// with a
	// token in it. The password is what follows the colon.
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/\s:@]+):([^/\s@]+)@`),
	// AWS-style key=value pairs in a message.
	regexp.MustCompile(`(?i)((?:secret|password|passwd|token|apikey|api_key|access_key|secretaccesskey)\s*[=:]\s*)("?)([^\s"',;)]+)`),
	// A bare AWS secret access key: 40 characters of base64 alphabet.
	regexp.MustCompile(`\b[A-Za-z0-9/+=]{40}\b`),
}

// RedactSecrets strips credentials out of a string that is about to be shown to
// someone or written down.
//
// Connection test failures were rendered to the browser and stored in
// last_check_error verbatim. The redaction helper existed and was used for the
// UI's preview of a connection string, but not for the errors — which is the
// one place a driver is most likely to quote the URI back.
func RedactSecrets(text string) string {
	if text == "" {
		return text
	}
	out := credentialPatterns[0].ReplaceAllString(text, "$1$2:•••@")
	out = credentialPatterns[1].ReplaceAllString(out, "$1$2•••")
	out = credentialPatterns[2].ReplaceAllString(out, "•••")
	return out
}

// RedactSecretsFromError is RedactSecrets over an error's message, returning ""
// for a nil error.
func RedactSecretsFromError(err error) string {
	if err == nil {
		return ""
	}
	return RedactSecrets(err.Error())
}

// ValidateDestination checks one backup account for the combinations that
// cannot work, before anything is stored or a job is pointed at it.
func ValidateDestination(d Destination) error {
	if d.Alias == "" {
		return fmt.Errorf("an alias is required")
	}

	switch d.Provider {
	case ProviderS3, ProviderOCI:
	case ProviderGDrive:
		return validateDriveDestination(d)
	default:
		return fmt.Errorf("unknown provider %q (want %q, %q or %q)", d.Provider, ProviderS3, ProviderOCI, ProviderGDrive)
	}
	if d.Bucket == "" {
		return fmt.Errorf("a bucket is required")
	}
	if d.Region == "" {
		return fmt.Errorf("a region is required")
	}
	if d.Provider == ProviderOCI && d.Endpoint == "" {
		return fmt.Errorf("Oracle Cloud needs its S3-compatible endpoint, e.g. https://<namespace>.compat.objectstorage.<region>.oraclecloud.com")
	}
	// AWS may supply credentials through an instance role, so explicit keys are
	// optional there; a custom endpoint has no such fallback.
	if d.Endpoint != "" && (d.AccessKeyID == "" || d.SecretAccessKey == "") {
		return fmt.Errorf("an access key and secret are required when a custom endpoint is set")
	}
	// Half a key pair is worse than none: it overrides the instance role and
	// then fails to sign, which surfaces as a permission error at upload time.
	if d.AccessKeyID != "" && d.SecretAccessKey == "" {
		return fmt.Errorf("an access key ID was given without its secret access key")
	}
	return nil
}

// validateDriveDestination checks a Google Drive account. Drive has no buckets
// or regions, and one of two ways to authenticate: a service account key, or an
// OAuth client. An OAuth client is savable before it has been authorised — it
// has to be, since the authorisation round trip starts from the saved account —
// and Ready reports whether that step still has to happen.
func validateDriveDestination(d Destination) error {
	hasServiceAccount := d.ServiceAccountJSON != ""
	hasOAuthClient := d.OAuthClientID != "" && d.OAuthClientSecret != ""

	switch {
	case hasServiceAccount && (hasOAuthClient || d.OAuthRefreshToken != ""):
		return fmt.Errorf("this destination has both a service account key and an OAuth client — keep one, since only one can be used")
	case hasServiceAccount, hasOAuthClient:
	case d.OAuthClientID != "" || d.OAuthClientSecret != "":
		return fmt.Errorf("an OAuth client needs both a client ID and a client secret")
	default:
		return fmt.Errorf("Google Drive needs either a service account key or an OAuth client ID and secret")
	}

	if hasServiceAccount {
		if !json.Valid([]byte(d.ServiceAccountJSON)) {
			return fmt.Errorf("the service account key is not valid JSON — paste the whole downloaded file")
		}
		// A service account owns no Drive storage of its own, so it can only
		// write into a shared drive. Left to itself that surfaces mid-run as a
		// quota error nobody expects.
		if d.DriveFolderID == "" {
			return fmt.Errorf("a folder ID is required with a service account — it has no My Drive of its own, so name a folder in a shared drive it can write to")
		}
	} else if d.DriveSubject != "" {
		return fmt.Errorf("impersonation is only possible with a service account key")
	}
	return nil
}

// Ready reports whether a destination has everything it needs to be used. It is
// false only for a Google Drive account whose OAuth client has not been
// authorised yet: the credentials are half there, so the account is stored but
// kept out of the live registry and out of the list a config can choose from.
func (d Destination) Ready() bool {
	if d.Provider != ProviderGDrive {
		return true
	}
	return d.ServiceAccountJSON != "" || d.OAuthRefreshToken != ""
}

func getString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return fallback
}

func getBool(key string, fallback bool) bool {
	if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return fallback
}
