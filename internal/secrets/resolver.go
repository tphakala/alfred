package secrets

import (
	"fmt"
	"reflect"
	"strings"
)

const (
	// secretRefPrefix is the start of a secret reference token.
	secretRefPrefix = "${secret:"
	// secretRefSuffix is the end of a secret reference token.
	secretRefSuffix = "}"
)

// IsSecretRef reports whether value is a secret reference of the form
// ${secret:name}.
func IsSecretRef(value string) bool {
	return strings.HasPrefix(value, secretRefPrefix) && strings.HasSuffix(value, secretRefSuffix)
}

// ExtractSecretName returns the secret name from a reference like ${secret:name}.
// If value is not a valid secret reference, the empty string is returned.
func ExtractSecretName(value string) string {
	if !IsSecretRef(value) {
		return ""
	}
	inner := value[len(secretRefPrefix) : len(value)-len(secretRefSuffix)]
	return inner
}

// ResolveSecretRefs walks all struct fields tagged with `secret:""` in cfg
// (which must be a non-nil pointer to a struct). If a field value is a
// ${secret:name} reference, it is replaced with the corresponding value from
// the secrets map. Fields that are not secret references are left unchanged.
//
// Returns a slice of warning strings for any references that could not be
// resolved (secret not found in map).
func ResolveSecretRefs(cfg any, secrets map[string]string) []string {
	if cfg == nil {
		return nil
	}
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return nil
	}
	return walkStruct(v, secrets)
}

// walkStruct recursively walks struct fields looking for the "secret" tag.
func walkStruct(v reflect.Value, secrets map[string]string) []string {
	var warnings []string
	t := v.Type()
	for field := range t.Fields() {
		fieldVal := v.FieldByIndex(field.Index)

		// Recurse into embedded or nested struct pointers.
		switch fieldVal.Kind() { //nolint:exhaustive // Only Struct and Pointer need recursion; all other kinds fall through.
		case reflect.Struct:
			warnings = append(warnings, walkStruct(fieldVal, secrets)...)
			continue
		case reflect.Pointer:
			if fieldVal.IsNil() || fieldVal.Elem().Kind() != reflect.Struct {
				continue
			}
			warnings = append(warnings, walkStruct(fieldVal.Elem(), secrets)...)
			continue
		}

		// Only process string fields with a "secret" tag.
		if _, ok := field.Tag.Lookup("secret"); !ok {
			continue
		}
		if fieldVal.Kind() != reflect.String {
			continue
		}
		if !fieldVal.CanSet() {
			continue
		}

		ref := fieldVal.String()
		if !IsSecretRef(ref) {
			// Backward compat: plain-text value in a secret-tagged field is left as-is.
			continue
		}

		name := ExtractSecretName(ref)
		secretVal, found := secrets[name]
		if !found {
			warnings = append(warnings, fmt.Sprintf("secret %q not found; field %q set to empty string", name, field.Name))
			fieldVal.SetString("")
			continue
		}
		fieldVal.SetString(secretVal)
	}
	return warnings
}
