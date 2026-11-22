//go:build bindings

package guiapp

// bindingsMode is set when the project is compiled as the wailsbindings
// binding-dump tool (run by `wails build` and `wails generate module`): with
// this tag App.Run dumps the JS bindings instead of starting the GUI, so the
// boot prologue must not chdir or touch any data files.
const bindingsMode = true
