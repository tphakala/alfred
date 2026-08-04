package main

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tphakala/alfred/internal/secrets"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

const (
	// keyFileSize is the number of random bytes for a generated key file.
	keyFileSize = 64

	// keyFilePermission is the file mode for generated key files.
	keyFilePermission = 0o400

	// configFilePermissionSecrets is the file mode for config files during migration.
	configFilePermissionSecrets = 0o600

	// secretsSetMaxArgs is the max arg count for `secrets set <name> [value]`.
	secretsSetMaxArgs = 2

	// yamlPathSplitParts is the max parts for dot-path splitting.
	yamlPathSplitParts = 2
)

// knownSecretFields maps YAML dot-path -> secret name for the migrate command.
var knownSecretFields = []struct {
	yamlPath   string
	secretName string
}{
	{"autotask.username", "autotask.username"},
	{"autotask.password", "autotask.password"},
	{"autotask.integration_code", "autotask.integration_code"},
	{"server.api_key", "server.api_key"},
	{"vertex_ai.credentials_file", "vertex_ai.credentials_file"},
	{"hindsight.url", "hindsight.url"},
}

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Manage the encrypted secrets store",
}

var secretsInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize the encrypted secrets store",
	RunE:  runSecretsInit,
}

var secretsSetCmd = &cobra.Command{
	Use:   "set <name> [value]",
	Short: "Set a secret value (prompts if value is omitted)",
	Args:  cobra.RangeArgs(1, secretsSetMaxArgs),
	RunE:  runSecretsSet,
}

var secretsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all secret names in the store",
	RunE:  runSecretsList,
}

var secretsDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a secret from the store",
	Args:  cobra.ExactArgs(1),
	RunE:  runSecretsDelete,
}

var secretsVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Cross-check the store against config secret references",
	RunE:  runSecretsVerify,
}

var secretsMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate plaintext YAML config values to the secrets store",
	RunE:  runSecretsMigrate,
}

var forceDelete bool

func init() {
	secretsDeleteCmd.Flags().BoolVar(&forceDelete, "force", false, "skip confirmation prompt")

	secretsCmd.AddCommand(
		secretsInitCmd,
		secretsSetCmd,
		secretsListCmd,
		secretsDeleteCmd,
		secretsVerifyCmd,
		secretsMigrateCmd,
	)
	rootCmd.AddCommand(secretsCmd)
}

// getSecretsPath returns the path to the encrypted secrets file.
func getSecretsPath() string {
	return filepath.Join(filepath.Dir(cfgFile), "secrets.enc.json")
}

// getDefaultKeyPath returns the path to the default key file.
func getDefaultKeyPath() string {
	return filepath.Join(filepath.Dir(cfgFile), "master.key")
}

// promptPassword reads a masked password from the terminal.
func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return string(b), nil
}

// loadExistingStore reads the salt from file and resolves the master key, then loads the store.
func loadExistingStore() (*secrets.SecretsStore, error) {
	path := getSecretsPath()
	salt, err := secrets.ReadSaltFromFile(path)
	if err != nil {
		return nil, err
	}
	if salt == nil {
		return nil, fmt.Errorf("secrets file %q contains empty KDF salt", path)
	}
	result, err := secrets.ResolveMasterKey(salt, getDefaultKeyPath())
	if err != nil {
		return nil, fmt.Errorf("resolving master key: %w", err)
	}
	store, err := secrets.LoadSecretsStore(path, result.Key)
	if err != nil {
		return nil, fmt.Errorf("loading secrets store: %w", err)
	}
	return store, nil
}

// loadOrInitStore loads an existing store or auto-initializes one if none exists.
func loadOrInitStore() (*secrets.SecretsStore, error) {
	path := getSecretsPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Println("No secrets store found. Initializing a new one...")
		return initializeStore()
	}
	return loadExistingStore()
}

// initializeStore prompts the user to choose passphrase or key file initialization.
func initializeStore() (*secrets.SecretsStore, error) {
	fmt.Println("Choose a key source for the secrets store:")
	fmt.Println("  1) Passphrase (PBKDF2-SHA256)")
	fmt.Println("  2) Key file (HKDF)")
	fmt.Print("Enter choice [1/2]: ")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	choice := strings.TrimSpace(scanner.Text())

	switch choice {
	case "1", "":
		return initWithPassphrase()
	case "2":
		return initWithKeyFile()
	default:
		return nil, fmt.Errorf("invalid choice %q: enter 1 or 2", choice)
	}
}

// initWithPassphrase creates a new secrets store using a passphrase.
func initWithPassphrase() (*secrets.SecretsStore, error) {
	pass, err := promptPassword("Enter passphrase: ")
	if err != nil {
		return nil, err
	}
	confirm, err := promptPassword("Confirm passphrase: ")
	if err != nil {
		return nil, err
	}
	if pass != confirm {
		return nil, fmt.Errorf("passphrases do not match")
	}
	if pass == "" {
		return nil, fmt.Errorf("passphrase must not be empty")
	}

	salt := make([]byte, secrets.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}

	key, err := secrets.DeriveKeyFromPassphrase(pass, salt)
	if err != nil {
		return nil, fmt.Errorf("deriving key from passphrase: %w", err)
	}

	store, err := secrets.NewSecretsStoreWithSalt(getSecretsPath(), key, salt, secrets.AlgorithmPBKDF2SHA256)
	if err != nil {
		return nil, fmt.Errorf("creating secrets store: %w", err)
	}
	if err := store.Save(); err != nil {
		return nil, fmt.Errorf("saving secrets store: %w", err)
	}
	fmt.Printf("Secrets store initialized at %s\n", getSecretsPath())
	return store, nil
}

// initWithKeyFile creates a new secrets store using a key file.
func initWithKeyFile() (*secrets.SecretsStore, error) {
	defaultKeyPath := getDefaultKeyPath()
	fmt.Printf("Key file path [%s]: ", defaultKeyPath)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	keyPath := strings.TrimSpace(scanner.Text())
	if keyPath == "" {
		keyPath = defaultKeyPath
	}

	// If no key file exists at the path, generate one.
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		fmt.Printf("Key file not found; generating a new one at %s\n", keyPath)
		keyMaterial := make([]byte, keyFileSize)
		if _, err := rand.Read(keyMaterial); err != nil {
			return nil, fmt.Errorf("generating key material: %w", err)
		}
		writeErr := os.WriteFile(keyPath, keyMaterial, keyFilePermission)
		clear(keyMaterial)
		if writeErr != nil {
			return nil, fmt.Errorf("writing key file: %w", writeErr)
		}
		fmt.Printf("Key file written to %s — keep this safe!\n", keyPath)
	}

	// Use a single fd for stat + read to avoid TOCTOU between permission check and read.
	f, err := os.Open(keyPath)
	if err != nil {
		return nil, fmt.Errorf("opening key file: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("checking key file permissions: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("key file %s is group/other-readable (mode %04o); must be 0400 or 0600", keyPath, perm)
	}

	const maxKeyFile = 1 << 20 // 1 MiB
	if info.Size() > maxKeyFile {
		return nil, fmt.Errorf("key file %s is too large (%d bytes)", keyPath, info.Size())
	}
	keyMaterial, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, fmt.Errorf("reading key file: %w", err)
	}
	defer clear(keyMaterial)
	key, err := secrets.DeriveKeyFromKeyFile(keyMaterial)
	if err != nil {
		return nil, fmt.Errorf("deriving key from key file: %w", err)
	}

	salt := make([]byte, secrets.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}

	store, err := secrets.NewSecretsStoreWithSalt(getSecretsPath(), key, salt, secrets.AlgorithmHKDFKeyFile)
	if err != nil {
		return nil, fmt.Errorf("creating secrets store: %w", err)
	}
	if err := store.Save(); err != nil {
		return nil, fmt.Errorf("saving secrets store: %w", err)
	}
	fmt.Printf("Secrets store initialized at %s\n", getSecretsPath())
	return store, nil
}

// runSecretsInit is the cobra handler for `alfred secrets init`.
func runSecretsInit(_ *cobra.Command, _ []string) error {
	path := getSecretsPath()
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("secrets store already exists at %s; delete it first to reinitialize", path)
	}
	_, err := initializeStore()
	return err
}

// runSecretsSet is the cobra handler for `alfred secrets set <name> [value]`.
func runSecretsSet(_ *cobra.Command, args []string) error {
	name := args[0]
	var value string
	if len(args) == secretsSetMaxArgs {
		value = args[1]
	} else {
		var err error
		value, err = promptPassword(fmt.Sprintf("Enter value for %q: ", name))
		if err != nil {
			return err
		}
	}

	store, err := loadOrInitStore()
	if err != nil {
		return err
	}
	if err := store.Set(name, value); err != nil {
		return fmt.Errorf("setting secret: %w", err)
	}
	if err := store.Save(); err != nil {
		return fmt.Errorf("saving store: %w", err)
	}
	fmt.Printf("Secret %q set.\n", name)
	return nil
}

// runSecretsList is the cobra handler for `alfred secrets list`.
func runSecretsList(_ *cobra.Command, _ []string) error {
	store, err := loadExistingStore()
	if err != nil {
		return err
	}
	names := store.Names()
	if len(names) == 0 {
		fmt.Println("No secrets stored.")
		return nil
	}
	for _, n := range names {
		fmt.Println(n)
	}
	return nil
}

// isSecretReferencedInConfig reports whether the secret name appears as a
// ${secret:NAME} reference in the config file.
func isSecretReferencedInConfig(configPath, name string) bool {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	ref := fmt.Sprintf("${secret:%s}", name)
	return strings.Contains(string(data), ref)
}

// runSecretsDelete is the cobra handler for `alfred secrets delete <name>`.
func runSecretsDelete(_ *cobra.Command, args []string) error {
	name := args[0]

	if !forceDelete {
		if isSecretReferencedInConfig(cfgFile, name) {
			return fmt.Errorf("secret %q is referenced in config %q; use --force to delete anyway", name, cfgFile)
		}
		fmt.Printf("Delete secret %q? [y/N]: ", name)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	store, err := loadExistingStore()
	if err != nil {
		return err
	}
	if !store.Delete(name) {
		return fmt.Errorf("secret %q not found", name)
	}
	if err := store.Save(); err != nil {
		return fmt.Errorf("saving store: %w", err)
	}
	fmt.Printf("Secret %q deleted.\n", name)
	return nil
}

// runSecretsVerify is the cobra handler for `alfred secrets verify`.
func runSecretsVerify(_ *cobra.Command, _ []string) error {
	store, err := loadExistingStore()
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		return fmt.Errorf("reading config %q: %w", cfgFile, err)
	}

	var cfgMap map[string]any
	if err := yaml.Unmarshal(raw, &cfgMap); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	var missing, ok []string
	for _, f := range knownSecretFields {
		val, found := getYAMLValue(cfgMap, f.yamlPath)
		if !found {
			continue
		}
		strVal, isStr := val.(string)
		if !isStr || !secrets.IsSecretRef(strVal) {
			continue
		}
		// It's a ${secret:name} reference — extract the actual name from the
		// reference string rather than using the static field name.
		refName := secrets.ExtractSecretName(strVal)
		if _, exists := store.Get(refName); exists {
			ok = append(ok, refName)
		} else {
			missing = append(missing, refName)
		}
	}

	if len(ok) > 0 {
		fmt.Println("OK:")
		for _, n := range ok {
			fmt.Printf("  + %s\n", n)
		}
	}
	if len(missing) > 0 {
		fmt.Println("MISSING in store:")
		for _, n := range missing {
			fmt.Printf("  - %s\n", n)
		}
		return fmt.Errorf("%d secret reference(s) in config have no matching entry in the store", len(missing))
	}
	if len(ok) == 0 && len(missing) == 0 {
		fmt.Println("No secret references found in config.")
	}
	return nil
}

// runSecretsMigrate is the cobra handler for `alfred secrets migrate`.
func runSecretsMigrate(_ *cobra.Command, _ []string) error {
	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		return fmt.Errorf("reading config %q: %w", cfgFile, err)
	}

	var cfgMap map[string]any
	if err := yaml.Unmarshal(raw, &cfgMap); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	store, err := loadOrInitStore()
	if err != nil {
		return err
	}

	migrated, err := migrateSecretFields(cfgMap, store)
	if err != nil {
		return err
	}

	if len(migrated) == 0 {
		fmt.Println("No plaintext secrets to migrate.")
		return nil
	}

	// Marshal the updated config back to YAML bytes first.
	out, err := yaml.Marshal(cfgMap)
	if err != nil {
		return fmt.Errorf("marshaling updated config: %w", err)
	}

	// Write config to a temp file so that if secrets save fails, the original
	// config is still intact.
	tmpCfg := cfgFile + ".tmp"
	if err := os.WriteFile(tmpCfg, out, configFilePermissionSecrets); err != nil {
		return fmt.Errorf("writing temp config: %w", err)
	}

	// Save the updated secrets store; clean up temp file on failure.
	if err := store.Save(); err != nil {
		_ = os.Remove(tmpCfg)
		return fmt.Errorf("saving secrets store: %w", err)
	}

	// Atomically replace the config file with the updated version.
	if err := os.Rename(tmpCfg, cfgFile); err != nil {
		return fmt.Errorf("replacing config file: %w", err)
	}

	fmt.Printf("Migrated %d secret(s):\n", len(migrated))
	for _, n := range migrated {
		fmt.Printf("  %s\n", n)
	}
	fmt.Printf("Config updated: %s\n", cfgFile)
	fmt.Printf("Secrets saved: %s\n", getSecretsPath())
	return nil
}

// migrateSecretFields iterates known secret fields and migrates plaintext values
// from cfgMap into the secrets store, returning the names of migrated secrets.
func migrateSecretFields(cfgMap map[string]any, store *secrets.SecretsStore) ([]string, error) {
	var migrated []string
	for _, f := range knownSecretFields {
		val, found := getYAMLValue(cfgMap, f.yamlPath)
		if !found {
			continue
		}
		strVal, isStr := val.(string)
		if !isStr || strVal == "" {
			continue
		}
		if strings.HasPrefix(strVal, "${") && !secrets.IsSecretRef(strVal) {
			continue
		}
		if secrets.IsSecretRef(strVal) {
			continue
		}
		if err := store.Set(f.secretName, strVal); err != nil {
			return nil, fmt.Errorf("storing secret %q: %w", f.secretName, err)
		}
		setYAMLValue(cfgMap, f.yamlPath, fmt.Sprintf("${secret:%s}", f.secretName))
		migrated = append(migrated, f.secretName)
	}
	return migrated, nil
}

// getYAMLValue retrieves a value from a nested map[string]any using a dot-separated path.
func getYAMLValue(m map[string]any, path string) (any, bool) {
	parts := strings.SplitN(path, ".", yamlPathSplitParts)
	val, ok := m[parts[0]]
	if !ok {
		return nil, false
	}
	if len(parts) == 1 {
		return val, true
	}
	nested, ok := val.(map[string]any)
	if !ok {
		return nil, false
	}
	return getYAMLValue(nested, parts[1])
}

// setYAMLValue sets a value in a nested map[string]any using a dot-separated path.
func setYAMLValue(m map[string]any, path string, value any) {
	parts := strings.SplitN(path, ".", yamlPathSplitParts)
	if len(parts) == 1 {
		m[parts[0]] = value
		return
	}
	nested, ok := m[parts[0]].(map[string]any)
	if !ok {
		nested = make(map[string]any)
		m[parts[0]] = nested
	}
	setYAMLValue(nested, parts[1], value)
}
