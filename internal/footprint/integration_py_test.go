package footprint

import "testing"

const pyBase = `import pkg.util
from . import helpers
from ..models import User


def run(path, strict=False):
    helpers.go(path)


def fetch(url):
    return url


def body():
    return 1


def old_name(a):
    return a * 2


def split(text, sep):
    return text


class Service:
    retries: int = 3
`

const pyHead = `import pkg.util
from . import helpers
from ..models import User
from .config import load as load_config
from app.core import engine


def run(path, strict=False, *, timeout=None):
    helpers.go(path)
    load_config(path)


def fetch(link):
    return link


def body():
    pkg.util.walk()
    engine.start()
    return User.create()


def new_name(a):
    return a * 2


def split(text, /, sep):
    return text


class Service:
    retries: int = 3
    name: str = "x"

    def start(self):
        self.log()
        super().start()
        make()[0].go()
`

func TestIntegrationPython(t *testing.T) {
	res := computeReal(t, "",
		change{status: "M", path: "pkg/sub/svc.py", base: pyBase, head: pyHead},
		change{status: "R", path: "pkg/old/__init__.py", oldPath: "pkg/old.py",
			base: "def f():\n    return 1\n", head: "def f():\n    return 1\n"},
	)
	checkLines(t, "symbols", summarize(res.Footprint.Symbols), []string{
		"pkg/old/__init__.py:f moved from pkg/old.py sig=false body=false", // same module pkg/old
		"pkg/sub/svc.py:Service signature",                                 // an optional member added
		"pkg/sub/svc.py:Service.start added",
		"pkg/sub/svc.py:body body",
		"pkg/sub/svc.py:fetch signature!", // url renamed to link
		"pkg/sub/svc.py:new_name renamed! from old_name",
		"pkg/sub/svc.py:run signature",    // a keyword-only optional parameter
		"pkg/sub/svc.py:split signature!", // text became positional-only
	})
	checkLines(t, "refs", summarizeRefs(res.Footprint.Refs), []string{
		"pkg/sub/svc.py:9 helpers.go pkg/sub/helpers",    // from . import helpers
		"pkg/sub/svc.py:10 load pkg/sub/config",          // from .config import load as load_config
		"pkg/sub/svc.py:18 pkg.util.walk pkg/util",       // import pkg.util
		"pkg/sub/svc.py:19 engine.start app/core/engine", // from app.core import engine
		"pkg/sub/svc.py:20 User.create pkg/models",       // from ..models import User
		"pkg/sub/svc.py:36 self.log ?",
		"pkg/sub/svc.py:37 super().start ?",
		"pkg/sub/svc.py:38 make()[].go ?",
		"pkg/sub/svc.py:38 make pkg/sub/svc",
	})
}

func TestIntegrationPythonOverloads(t *testing.T) {
	const base = "from typing import overload\n\n@overload\ndef f(a: int) -> int: ...\n@overload\ndef f(a: str) -> str: ...\ndef f(a):\n    return a\n"
	const head = "from typing import overload\n\n@overload\ndef f(a: int) -> int: ...\n@overload\ndef f(a: bytes) -> bytes: ...\ndef f(a):\n    return a\n"
	res := computeReal(t, "", change{status: "M", path: "m.py", base: base, head: head})
	checkLines(t, "symbols", summarize(res.Footprint.Symbols), []string{"m.py:f signature!"})
}
