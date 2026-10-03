package script

import (
	"fmt"
	"net/http/cookiejar"

	"github.com/dop251/goja"
)

// Inspect evaluates a script's source just far enough to read its runtime
// manifest (Transport.info()) and returns it, WITHOUT ever calling open() or
// touching the network. It is the read an app does before a transport is
// trusted or connected: it surfaces the name/version and the params a user
// must fill.
//
// Inspect does NOT verify the signature - that is a separate decision the
// caller makes with VerifyScript (or RawPackage.Verify for a .flux), so the
// UI can show "what this claims to be" and "whether it is signed by a key you
// trust" as two distinct facts. Running info() on an untrusted script is safe:
// it returns a literal object, and every side-effecting host call
// (http.fetch, ws.open, emit, setState) fires only from open()/write(), which
// Inspect never invokes.
func Inspect(src []byte) (Info, error) {
	jar, _ := cookiejar.New(nil)
	t := &ScriptTransport{name: "inspect", cookieJar: jar}

	vm := goja.New()
	registerHostAPI(vm, t)
	if _, err := vm.RunString(string(src)); err != nil {
		return Info{}, fmt.Errorf("eval: %w", err)
	}
	obj, ok := vm.Get("Transport").(*goja.Object)
	if !ok || obj == nil {
		return Info{}, fmt.Errorf("script must define a global `Transport` object")
	}
	infoFn, ok := goja.AssertFunction(obj.Get("info"))
	if !ok {
		return Info{}, fmt.Errorf("Transport.info is not a function")
	}
	infoVal, err := infoFn(obj)
	if err != nil {
		return Info{}, fmt.Errorf("Transport.info(): %w", err)
	}
	return parseInfo(infoVal)
}
