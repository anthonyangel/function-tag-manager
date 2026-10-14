package main

import (
	"slices"

	"github.com/crossplane-contrib/function-tag-manager/filters"
	"github.com/crossplane-contrib/function-tag-manager/input/v1beta1"
	"github.com/crossplane/function-sdk-go/resource"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/fieldpath"
)

// ManagedField returns the name of the spec.forProvider field that holds tags
// for a resource, and true if the resource supports tags.
func ManagedField(desired *resource.DesiredComposed, filter filters.ResourceFilter) (string, bool) {
	if field, ok := filter[groupKind(desired)]; ok {
		return field, true
	}

	// Filter out any remaining resources
	return "", false
}

// groupKind returns the GroupKind of a resource, in the form used by filters.
func groupKind(desired *resource.DesiredComposed) string {
	gvk := desired.Resource.GroupVersionKind()

	return gvk.Group + "/" + gvk.Kind
}

// ManagedRules returns the rules for the tags of a resource, and true if its
// provider places any limits on them.
func ManagedRules(desired *resource.DesiredComposed, rules filters.ResourceRules) (filters.Rules, bool) {
	r, ok := rules[groupKind(desired)]
	return r, ok
}

// ValidateTags checks the tags that the Function is about to add to, and
// remove from, a resource against the rules of its provider. The field is the
// name of the spec.forProvider field that holds tags.
//
// Only the tags that are being added are checked individually, because tags that
// are already on the resource weren't set by this Function. The total number of
// tags the resource will have after the update is always checked.
func ValidateTags(desired *resource.DesiredComposed, rules filters.Rules, field string, adding []v1beta1.Tags, removing []string) error {
	final := v1beta1.Tags{}

	_ = fieldpath.Pave(desired.Resource.Object).GetValueInto("spec.forProvider."+field, &final)

	for _, tags := range adding {
		keys := make([]string, 0, len(tags))
		for k := range tags {
			keys = append(keys, k)
		}

		// Sort the keys so the error is the same each time.
		slices.Sort(keys)

		for _, k := range keys {
			if rules.ValidateTag != nil {
				err := rules.ValidateTag(k, tags[k])
				if err != nil {
					return errors.Wrapf(err, "cannot add tag %q", k)
				}
			}

			final[k] = tags[k]
		}
	}

	for _, k := range removing {
		delete(final, k)
	}

	if rules.MaxTags > 0 && len(final) > rules.MaxTags {
		return errors.Errorf("the resource would have %d tags, more than the limit of %d", len(final), rules.MaxTags)
	}

	return nil
}
