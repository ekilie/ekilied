package recipes

type Recipe interface {
	Name() string
	Execute(params map[string]interface{}) error
}
