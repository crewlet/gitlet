package footprint

import "testing"

const tsBase = `import { load } from "./config";
import * as api from "../api";
import React from "react";

export function run(path: string): void {
  load(path);
}

export function strict(path: string): void {
  api.get(path);
}

export function body(): number {
  return 1;
}

function oldName(a: number): number {
  return a * 2;
}

export interface Opts {
  name: string;
}
`

const tsHead = `import { load, parse as p } from "./config";
import * as api from "../api";
import React from "react";

export function run(path: string, verbose?: boolean): void {
  load(path);
  p(path);
}

export function strict(path: string, mode: string): void {
  api.post(path);
}

export function body(): number {
  return compute(2) + React.version.length + make().size();
}

function newName(a: number): number {
  return a * 2
}

export interface Opts {
  name: string;
  port?: number;
}
`

const tsxBase = `import { Button } from "./button";

export function View(props: Props) {
  return <div />;
}
`

const tsxHead = `import { Button } from "./button";
import * as ui from "./ui";
import { default as Card } from "../card";

export function View(props: Props) {
  return <div><Button /><ui.Panel /><Card /></div>;
}
`

func TestIntegrationTypeScript(t *testing.T) {
	res := computeReal(t, "",
		change{status: "M", path: "src/app/main.ts", base: tsBase, head: tsHead},
		change{status: "M", path: "src/app/view.tsx", base: tsxBase, head: tsxHead},
		change{status: "R", path: "lib/x/index.js", oldPath: "lib/x.js",
			base: "export function helper(a) {\n  return a;\n}\n", head: "export function helper(a, b = 1) {\n  return a;\n}\n"},
		change{status: "R", path: "lib/y.js", oldPath: "lib/y/index.js",
			base: "export function other() {}\n", head: "export function other() {}\n"},
	)
	checkLines(t, "symbols", summarize(res.Footprint.Symbols), []string{
		// A defaulted parameter appended, and a.js to a/index.js keeps module a.
		"lib/x/index.js:helper moved from lib/x.js sig=true body=true",
		// y/index.js to y.js loses the module y/index.
		"lib/y.js:other moved! from lib/y/index.js sig=false body=false",
		"src/app/main.ts:Opts signature", // an optional member added
		"src/app/main.ts:body body",
		// The renamed body also lost its semicolon, which formatters own.
		"src/app/main.ts:newName renamed! from oldName",
		"src/app/main.ts:run signature", // an optional parameter appended
		"src/app/main.ts:strict signature!",
		"src/app/view.tsx:View body",
	})
	checkLines(t, "refs", summarizeRefs(res.Footprint.Refs), []string{
		"src/app/main.ts:6 load src/app/config",
		"src/app/main.ts:7 parse src/app/config", // through the alias p
		"src/app/main.ts:11 api.post src/api",
		"src/app/main.ts:15 compute src/app/main",
		"src/app/main.ts:15 make src/app/main",
		"src/app/main.ts:15 <expr>.size ?",
		"src/app/view.tsx:5 Props src/app/view",
		"src/app/view.tsx:6 Button src/app/button", // JSX tags are calls
		"src/app/view.tsx:6 Card src/card",         // { default as Card }
		"src/app/view.tsx:6 ui.Panel src/app/ui",
	})
}

// A `new` of a default import, a CommonJS require binding or a TypeScript
// `import = require()` builds what the module exports, whatever the
// importer calls it.
func TestIntegrationTypeScriptNewOfAnExport(t *testing.T) {
	res := computeReal(t, "",
		change{status: "A", path: "src/a.ts", head: "import Cfg from './config';\nnew Cfg('x');\n"},
		change{status: "A", path: "src/b.js", head: "const Cfg = require('./config');\nnew Cfg('x');\n"},
		change{status: "A", path: "src/c.ts", head: "import Cfg = require('./config');\nnew Cfg('x');\n"},
		change{status: "A", path: "src/d.ts", head: "import { Config as C } from './config';\nnew C('x');\n"},
	)
	checkLines(t, "refs", summarizeRefs(res.Footprint.Refs), []string{
		"src/a.ts:2 Cfg src/config default export",
		"src/b.js:2 Cfg src/config default export",
		"src/c.ts:2 Cfg src/config default export",
		"src/d.ts:2 Config src/config", // a named export goes by its own name
	})
}

func TestIntegrationTypeScriptVisibility(t *testing.T) {
	const base = "export class Service {\n  private log(): void {}\n  protected tick(): void {}\n  run(): void {}\n}\n"
	const head = "export class Service {\n  protected log(): void {}\n  private tick(): void {}\n  run(a?: string): void {}\n}\n"
	res := computeReal(t, "", change{status: "M", path: "src/svc.ts", base: base, head: head})
	checkLines(t, "symbols", summarize(res.Footprint.Symbols), []string{
		"src/svc.ts:Service.log signature",   // private to protected
		"src/svc.ts:Service.run signature",   // optional parameter
		"src/svc.ts:Service.tick signature!", // protected to private
	})
}
