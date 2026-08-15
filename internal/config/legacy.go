package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// Legacy is what `feather import` reads out of the old file-based setup: the
// MongoDB account and the backup destinations that used to live in .env. Once
// imported into the database, those variables stop being read.
type Legacy struct {
	Sources      map[string]MongoSource
	Destinations map[string]Destination
}

// LoadLegacyEnv parses an old-style env file without touching the process
// environment, so importing does not change how the running daemon behaves.
func LoadLegacyEnv(path string) (*Legacy, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s does not exist", path)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	legacy := &Legacy{
		Sources:      map[string]MongoSource{},
		Destinations: map[string]Destination{},
	}

	if uri := strings.TrimSpace(values["MONGO_URI"]); uri != "" {
		alias := valueOr(values, "MONGO_ALIAS", "default")
		legacy.Sources[alias] = MongoSource{
			Alias:      alias,
			URI:        uri,
			AuthSource: valueOr(values, "MONGO_AUTH_SOURCE", "admin"),
			AppName:    valueOr(values, "MONGO_APP_NAME", "feather"),
		}
	}

	for _, alias := range splitList(values["FEATHER_DESTINATIONS"]) {
		prefix := "DEST_" + envKeySegment(alias) + "_"
		provider := Provider(strings.ToLower(valueOr(values, prefix+"PROVIDER", string(ProviderS3))))

		dest := Destination{
			Alias:           alias,
			Provider:        provider,
			Region:          values[prefix+"REGION"],
			Bucket:          values[prefix+"BUCKET"],
			Endpoint:        values[prefix+"ENDPOINT"],
			AccessKeyID:     values[prefix+"ACCESS_KEY_ID"],
			SecretAccessKey: values[prefix+"SECRET_ACCESS_KEY"],
			SessionToken:    values[prefix+"SESSION_TOKEN"],
			StorageClass:    values[prefix+"STORAGE_CLASS"],
			Prefix:          strings.TrimPrefix(values[prefix+"PREFIX"], "/"),
			// Oracle's S3 compatibility layer is path-style only.
			ForcePathStyle: provider == ProviderOCI || strings.EqualFold(values[prefix+"FORCE_PATH_STYLE"], "true"),
		}
		if err := ValidateDestination(dest); err != nil {
			return nil, fmt.Errorf("destination %q in %s: %w", alias, path, err)
		}
		legacy.Destinations[alias] = dest
	}

	return legacy, nil
}

// envKeySegment turns an alias into the env key fragment the old layout used:
// "s3-backup-acct" reads DEST_S3_BACKUP_ACCT_*.
func envKeySegment(alias string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(alias) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func valueOr(values map[string]string, key, fallback string) string {
	if v := strings.TrimSpace(values[key]); v != "" {
		return v
	}
	return fallback
}
