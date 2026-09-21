package recipes

// Recipe describes an executable job recipe.
// Implemented by: nothing. No implementations exist; this package is legacy
// scaffolding and is slated for removal.
type Recipe interface {
	Name() string
	Execute(params map[string]interface{}) error
}
