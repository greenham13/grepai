package framework

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture shared by every shape test: script, Vue 2 filter template, scoped
// style with a v-bind. Line numbers below refer to this text.
const vue2FilterSFC = `<template>
  <div class="card">
    <span>{{ price | currency }}</span>
    <button @click="buy(item)">{{ label }}</button>
  </div>
</template>

<script>
export function buy(item) {
  return item.id
}
export default {
  name: 'Vue2Filter',
}
</script>

<style scoped>
.card { color: v-bind(color); }
</style>
`

const vue3SetupSFC = `<template>
  <div class="card">{{ formatted(price) }}</div>
</template>

<script lang="ts">
export function helper(n: number): number {
  return n * 2
}
</script>

<script setup lang="ts">
const price = 1
function formatted(v: number) { return '$' + helper(v) }
</script>
`

// fakeVue27Compiler mirrors @vue/compiler-sfc 2.7.16 as probed on 2026-10-13:
// parse(options) returns the descriptor itself, blocks carry start/end
// offsets, parse(string) returns a descriptor with every block null instead
// of throwing, compileTemplate reads `_vm.*` and reports {msg,start,end}.
const fakeVue27Compiler = `
function block(source, tag) {
  const re = new RegExp("<" + tag + "[^>]*>([\\s\\S]*?)</" + tag + ">");
  const m = re.exec(source);
  if (!m) return null;
  const start = m.index + m[0].indexOf(">") + 1;
  return { type: tag, content: m[1], start, end: start + m[1].length, attrs: {}, scoped: m[0].includes("scoped") };
}
function empty() {
  return { source: "", filename: "", template: null, script: null, scriptSetup: null, styles: [], customBlocks: [], cssVars: [], errors: [], shouldForceReload: () => false };
}
export function parse(options) {
  if (typeof options !== "object" || options === null) return empty();
  const s = options.source;
  const style = block(s, "style");
  return { source: s, filename: options.filename, template: block(s, "template"), script: block(s, "script"), scriptSetup: null,
    styles: style ? [style] : [], customBlocks: [], cssVars: [], errors: [], shouldForceReload: () => false };
}
export function compileTemplate(options) {
  if (options.source.includes("BREAK")) {
    return { ast: null, code: "", source: options.source, tips: [], errors: [{ msg: "invalid expression: BREAK", start: 0, end: 5 }] };
  }
  return { ast: null, source: options.source, tips: [], errors: [],
    code: "var render = function render() {\n  var _vm = this,\n    _c = _vm._self._c\n  return _c('span', [_vm._v(_vm._s(_vm._f('currency')(_vm.price))), _vm.buy(_vm.item)])\n}" };
}
`

// fakeVue3Compiler mirrors @vue/compiler-sfc 3.5.44: parse(source, options)
// returns {descriptor, errors}, blocks carry loc.start.{line,column,offset},
// parse(object) throws, template expression errors are reported by parse()
// as well as by compileTemplate(), and render code reads `_ctx.*`.
const fakeVue3Compiler = `
function lineOf(source, offset) { let line = 1; for (let i = 0; i < offset; i++) if (source[i] === "\n") line++; return line; }
function block(source, tag) {
  const re = new RegExp("<" + tag + "[^>]*>([\\s\\S]*?)</" + tag + ">", "g");
  const out = [];
  let m;
  while ((m = re.exec(source))) {
    const offset = m.index + m[0].indexOf(">") + 1;
    out.push({ type: tag, content: m[1], attrs: {}, scoped: m[0].includes("scoped"), setup: m[0].includes("setup"),
      loc: { start: { line: lineOf(source, offset), column: 1, offset }, end: { line: 0, column: 0, offset: offset + m[1].length }, source: m[1] } });
  }
  return out;
}
export function parse(source, options) {
  if (typeof source !== "string") throw new TypeError("currentInput.slice is not a function");
  const scripts = block(source, "script");
  const template = block(source, "template")[0] ?? null;
  const errors = [];
  if (template && template.content.includes("BREAK")) errors.push(new SyntaxError('Error parsing JavaScript expression: Unexpected token, expected "," (1:17)'));
  return { descriptor: { filename: options?.filename, source, template, script: scripts.find(s => !s.setup) ?? null,
    scriptSetup: scripts.find(s => s.setup) ?? null, styles: block(source, "style"), customBlocks: [], cssVars: [] }, errors };
}
export function compileTemplate(options) {
  if (options.source.includes("BREAK")) {
    return { code: "", ast: null, preamble: "", source: options.source, tips: [], map: undefined,
      errors: [new SyntaxError('Error parsing JavaScript expression: Unexpected token, expected "," (1:17)')] };
  }
  return { ast: null, preamble: "", source: options.source, tips: [], errors: [], map: undefined,
    code: "export function render(_ctx, _cache) {\n  return _toDisplayString(_ctx.formatted(_ctx.price)) + _ctx.buy(_ctx.item)\n}" };
}
`

func writeFakeCompiler(t *testing.T, projectDir, version, module string) {
	t.Helper()
	dir := filepath.Join(projectDir, "node_modules", "@vue", "compiler-sfc")
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := `{"name":"@vue/compiler-sfc","version":"` + version + `","type":"module","exports":{".":"./dist/index.mjs"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "index.mjs"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeComponent(t *testing.T, projectDir, name, source string) string {
	t.Helper()
	file := filepath.Join(projectDir, "src", name)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// sourceLineOf returns the source line the generated line holding `needle`
// maps to, and fails when the generated text does not contain it.
func sourceLineOf(t *testing.T, res TransformResult, needle string) int {
	t.Helper()
	for i, line := range strings.Split(res.Text, "\n") {
		if strings.Contains(line, needle) {
			return RemapLine(res.GeneratedToSourceLine, i+1)
		}
	}
	t.Fatalf("generated text has no line containing %q:\n%s", needle, res.Text)
	return 0
}

func lineInSource(t *testing.T, source, needle string) int {
	t.Helper()
	for i, line := range strings.Split(source, "\n") {
		if strings.Contains(line, needle) {
			return i + 1
		}
	}
	t.Fatalf("fixture has no line containing %q", needle)
	return 0
}

func assertScriptAndTemplate(t *testing.T, res TransformResult, source string, reads ...string) {
	t.Helper()
	for _, want := range []string{"export function buy(item)", "function __vue_template_reads__() {", "__css_v_bind__(color);"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("missing %q in:\n%s", want, res.Text)
		}
	}
	for _, name := range reads {
		if !strings.Contains(res.Text, "\n  "+name+"();") {
			t.Fatalf("template read %q missing in:\n%s", name, res.Text)
		}
	}
	for _, internal := range []string{"_self();", "_v();", "_s();", "_f();"} {
		if strings.Contains(res.Text, internal) {
			t.Fatalf("internal render helper leaked as a read %q:\n%s", internal, res.Text)
		}
	}
	if len(res.GeneratedToSourceLine) != len(strings.Split(res.Text, "\n")) {
		t.Fatalf("line map has %d entries for %d generated lines", len(res.GeneratedToSourceLine), len(strings.Split(res.Text, "\n")))
	}
	for _, needle := range []string{"export function buy(item)", "return item.id", "name: 'Vue2Filter'", ".card { color: v-bind(color); }"} {
		if got, want := sourceLineOf(t, res, needle), lineInSource(t, source, needle); got != want {
			t.Fatalf("%q maps to source line %d, want %d", needle, got, want)
		}
	}
}

func TestVueProcessorTransform_Vue27CompilerShape(t *testing.T) {
	project := filepath.Join(t.TempDir(), "vue2")
	writeFakeCompiler(t, project, "2.7.16", fakeVue27Compiler)
	file := writeComponent(t, project, "Card.vue", vue2FilterSFC)

	res, err := NewVueProcessor("node").TransformForEmbedding(context.Background(), file, vue2FilterSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	if !strings.Contains(res.Text, "var render = function render()") {
		t.Fatalf("Vue 2 render code missing:\n%s", res.Text)
	}
	assertScriptAndTemplate(t, res, vue2FilterSFC, "buy", "item", "price")
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
}

func TestVueProcessorTransform_Vue3CompilerShape(t *testing.T) {
	project := filepath.Join(t.TempDir(), "vue3")
	writeFakeCompiler(t, project, "3.5.44", fakeVue3Compiler)
	file := writeComponent(t, project, "Card.vue", vue2FilterSFC)

	res, err := NewVueProcessor("node").TransformForEmbedding(context.Background(), file, vue2FilterSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	if !strings.Contains(res.Text, "export function render(_ctx, _cache)") {
		t.Fatalf("Vue 3 render code missing:\n%s", res.Text)
	}
	assertScriptAndTemplate(t, res, vue2FilterSFC, "buy", "item", "price")

	setup := writeComponent(t, project, "Setup.vue", vue3SetupSFC)
	res, err = NewVueProcessor("node").TransformForEmbedding(context.Background(), setup, vue3SetupSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	for _, needle := range []string{"export function helper(n: number)", "return n * 2", "const price = 1", "function formatted(v: number)"} {
		if got, want := sourceLineOf(t, res, needle), lineInSource(t, vue3SetupSFC, needle); got != want {
			t.Fatalf("%q maps to source line %d, want %d", needle, got, want)
		}
	}
}

func TestVueProcessorTransform_UnknownCompilerVersionUsesReturnedShape(t *testing.T) {
	project := filepath.Join(t.TempDir(), "unknown")
	writeFakeCompiler(t, project, "", fakeVue27Compiler)
	file := writeComponent(t, project, "Card.vue", vue2FilterSFC)

	res, err := NewVueProcessor("node").TransformForEmbedding(context.Background(), file, vue2FilterSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	assertScriptAndTemplate(t, res, vue2FilterSFC, "buy", "item", "price")
}

func TestVueProcessorTransform_TemplateErrorKeepsScriptAndStyle(t *testing.T) {
	broken := strings.Replace(vue2FilterSFC, "{{ label }}", "{{ label | BREAK 20 }}", 1)
	for _, tc := range []struct{ name, version, module, want string }{
		{"vue3", "3.5.44", fakeVue3Compiler, `Error parsing JavaScript expression: Unexpected token, expected "," (1:17)`},
		{"vue27", "2.7.16", fakeVue27Compiler, "invalid expression: BREAK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := filepath.Join(t.TempDir(), tc.name)
			writeFakeCompiler(t, project, tc.version, tc.module)
			file := writeComponent(t, project, "Broken.vue", broken)

			res, err := NewVueProcessor("node").TransformForEmbedding(context.Background(), file, broken)
			if err != nil {
				t.Fatalf("a template error must not fail the file: %v", err)
			}
			if !strings.Contains(res.Text, "export function buy(item)") || !strings.Contains(res.Text, "__css_v_bind__(color);") {
				t.Fatalf("script or style output lost:\n%s", res.Text)
			}
			if strings.Contains(res.Text, "render") || strings.Contains(res.Text, "__vue_template_reads__") {
				t.Fatalf("template output should be skipped:\n%s", res.Text)
			}
			if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "vue template skipped for "+file+": "+tc.want) {
				t.Fatalf("want one template-skipped warning with %q, got %v", tc.want, res.Warnings)
			}
			if got, want := sourceLineOf(t, res, "return item.id"), lineInSource(t, broken, "return item.id"); got != want {
				t.Fatalf("script maps to %d, want %d", got, want)
			}
		})
	}
}

func TestVueProcessorTransform_MissingCompilerScopedToDirectoryChain(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "bare")
	equipped := filepath.Join(root, "equipped")
	writeFakeCompiler(t, equipped, "2.7.16", fakeVue27Compiler)
	bareFile := writeComponent(t, bare, "A.vue", vue2FilterSFC)
	equippedFile := writeComponent(t, equipped, "B.vue", vue2FilterSFC)
	nested := filepath.Join(bare, "src", "nested")
	writeFakeCompiler(t, nested, "2.7.16", fakeVue27Compiler)
	nestedFile := writeComponent(t, nested, "C.vue", vue2FilterSFC)

	p := NewVueProcessor("node")
	res, err := p.TransformForEmbedding(context.Background(), bareFile, vue2FilterSFC)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("bare project should report the compiler missing, got err=%v", err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "Cannot resolve @vue/compiler-sfc from "+filepath.Dir(bareFile)) {
		t.Fatalf("missing-compiler warning expected, got %v", res.Warnings)
	}
	if _, ok := p.missingCompilerMessage(bareFile); !ok {
		t.Fatal("bare project directory should be remembered as missing")
	}
	if _, ok := p.missingCompilerMessage(filepath.Join(root, "x.vue")); !ok {
		t.Fatal("ancestor directories were walked by resolution and must be remembered too")
	}
	if _, ok := p.missingCompilerMessage(equippedFile); ok {
		t.Fatal("sibling project must not inherit the missing mark")
	}
	if _, ok := p.missingCompilerMessage(nestedFile); ok {
		t.Fatal("a subdirectory with its own node_modules must not inherit the missing mark")
	}

	res, err = p.TransformForEmbedding(context.Background(), equippedFile, vue2FilterSFC)
	if err != nil {
		t.Fatalf("sibling project with a compiler must still compile: %v", err)
	}
	if !strings.Contains(res.Text, "var render = function render()") {
		t.Fatalf("sibling project lost compiled output:\n%s", res.Text)
	}
	res, err = p.TransformForEmbedding(context.Background(), nestedFile, vue2FilterSFC)
	if err != nil || !strings.Contains(res.Text, "var render = function render()") {
		t.Fatalf("nested project with its own compiler must compile: err=%v text=%q", err, res.Text)
	}

	res, err = p.TransformForEmbedding(context.Background(), filepath.Join(bare, "src", "D.vue"), vue2FilterSFC)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(res.Text, "export function buy(item)") {
		t.Fatalf("bare project should keep using the fallback: err=%v", err)
	}
}

func TestVueProcessorFallback_LineMapExact(t *testing.T) {
	res, err := NewVueProcessor("node").fallback("Comp.vue", vue3SetupSFC+"<style scoped>\n.a { color: v-bind(tone); }\n</style>\n")
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if len(res.GeneratedToSourceLine) != len(strings.Split(res.Text, "\n")) {
		t.Fatalf("line map has %d entries for %d generated lines:\n%s", len(res.GeneratedToSourceLine), len(strings.Split(res.Text, "\n")), res.Text)
	}
	for _, needle := range []string{"export function helper(n: number)", "const price = 1", "function formatted(v: number)"} {
		if got, want := sourceLineOf(t, res, needle), lineInSource(t, vue3SetupSFC, needle); got != want {
			t.Fatalf("%q maps to source line %d, want %d", needle, got, want)
		}
	}
	if got := sourceLineOf(t, res, "__css_v_bind__(tone);"); got != 16 {
		t.Fatalf("style binding maps to %d, want 16", got)
	}
}

// Real compilers: GREPAI_VUE_COMPILER_DIRS lists project directories (colon
// separated) whose node_modules carry @vue/compiler-sfc; each one's
// node_modules is linked into a temp project and the fixture is compiled.
func TestVueProcessorTransform_RealCompilers(t *testing.T) {
	dirs := os.Getenv("GREPAI_VUE_COMPILER_DIRS")
	if dirs == "" {
		t.Skip("set GREPAI_VUE_COMPILER_DIRS=<project>:<project> to run against installed @vue/compiler-sfc packages")
	}
	broken := strings.Replace(vue2FilterSFC, "{{ label }}", "{{ label | truncate 20 }}", 1)
	for _, dir := range strings.Split(dirs, ":") {
		pkg, err := os.ReadFile(filepath.Join(dir, "node_modules", "@vue", "compiler-sfc", "package.json"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		var meta struct{ Version string }
		if err := json.Unmarshal(pkg, &meta); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		t.Run(filepath.Base(dir), func(t *testing.T) {
			project := filepath.Join(t.TempDir(), "project")
			if err := os.MkdirAll(project, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "node_modules"), filepath.Join(project, "node_modules")); err != nil {
				t.Fatal(err)
			}
			file := writeComponent(t, project, "Card.vue", vue2FilterSFC)
			res, err := NewVueProcessor("node").TransformForEmbedding(context.Background(), file, vue2FilterSFC)
			if err != nil {
				t.Fatalf("transform failed: %v", err)
			}
			assertScriptAndTemplate(t, res, vue2FilterSFC, "buy", "item", "price", "label")
			t.Logf("@vue/compiler-sfc %s: %d generated lines, warnings %v", meta.Version, len(strings.Split(res.Text, "\n")), res.Warnings)

			brokenFile := writeComponent(t, project, "Broken.vue", broken)
			res, err = NewVueProcessor("node").TransformForEmbedding(context.Background(), brokenFile, broken)
			if err != nil {
				t.Fatalf("broken template must not fail the file: %v", err)
			}
			if !strings.Contains(res.Text, "export function buy(item)") {
				t.Fatalf("script lost on broken template:\n%s", res.Text)
			}
			t.Logf("broken template warnings: %v", res.Warnings)
		})
	}
}
