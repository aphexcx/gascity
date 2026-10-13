#!/usr/bin/env python3
"""Compare gc's parent selection with executable parsers from a beads checkout.

Usage: python3 scripts/check-bd-parent-grammar.py /path/to/beads
The checkout is read-only. Extracted bd functions run with their actual type
constants and validators; only package qualifiers and DTOs are localized.
"""
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile


def function(source, name):
    match = re.search(r"^func (?:\(d DependencyType\) )?" + name + r"\(.*?^}\n", source, re.M | re.S)
    if not match:
        raise RuntimeError(f"bd parser function missing: {name}")
    return match.group()


def main():
    beads = Path(sys.argv[1]).resolve()
    root = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip())
    deps = (beads / "cmd/bd/create_deps.go").read_text()
    types = (beads / "internal/types/types.go").read_text()
    create = (beads / "cmd/bd/create.go").read_text()
    constants = re.findall(r'^\s*(\w+\s+DependencyType\s*=\s*"[^"]+")', types, re.M)
    maxlen = re.search(r'^const MaxDependencyTypeLen = \d+', types, re.M)
    if not constants or not maxlen:
        raise RuntimeError("bd dependency constants changed shape")
    alias_body = function(deps, "canonicalDependencyType")
    candidates = set(re.findall(r'"([a-z][a-z-]*)"', "\n".join(constants) + alias_body))
    candidates |= {s.upper() for s in candidates} | {s.title() for s in candidates}
    raw = [f"{kind}{sep}parent-1" for kind in sorted(candidates) for sep in (":", "\t:\t", " : ")]
    funcs = [function(deps, n) for n in ("parseDepSpec", "canonicalDependencyType", "validateDependencyType")]
    funcs += [function(types, n) for n in ("IsValid", "IsWellKnown", "WellKnownDependencyTypes")]
    funcs += [function(create, "createDepsAcceptedTypeList")]
    source = '''package main
import ("encoding/json"; "fmt"; "os"; "sort"; "strings")
type DependencyType string
type DependencySpec struct { Type DependencyType; TargetID string; SwapDirection bool }
'''
    source += "const (\n" + "\n".join(constants) + "\n)\n" + maxlen.group() + "\n"
    source += "\n".join(funcs).replace("types.", "").replace("domain.", "")
    source += '''
func main() {
 var raw []string
 if err:=json.NewDecoder(os.Stdin).Decode(&raw); err!=nil {panic(err)}
 out:=map[string]bool{}
 for _,s:=range raw { p,err:=parseDepSpec(strings.TrimSpace(s)); out[s]=err==nil && p.Type==DepParentChild }
 if err:=json.NewEncoder(os.Stdout).Encode(out); err!=nil {panic(err)}
}
'''
    # Keep compile scratch on disk, never in a repository or a shared /tmp cache.
    with tempfile.TemporaryDirectory(prefix="bd-parent-grammar-", dir="/var/tmp") as scratch:
        scratch = Path(scratch)
        oracle = scratch / "oracle.go"
        oracle.write_text(source)
        result = subprocess.run(["go", "run", str(oracle)], input=json.dumps(raw), text=True, capture_output=True, check=True)
        expected = json.loads(result.stdout)
        test = scratch / "grammar_test.go"
        test.write_text('''package main
import ("encoding/json"; "testing")
func TestBdParentGrammarAgainstSource(t *testing.T) {
 var expected map[string]bool
 if err:=json.Unmarshal([]byte(''' + json.dumps(result.stdout) + '''), &expected); err!=nil {t.Fatal(err)}
 for raw,want:=range expected {
  got:=len(bdCreateParents([]string{"create","child","--deps",raw}))>0
  if got!=want {t.Errorf("dependency %q: gc parent=%v, bd parent=%v",raw,got,want)}
 }
}
''')
        overlay = scratch / "overlay.json"
        overlay.write_text(json.dumps({"Replace": {str(root / "cmd/gc/bd_parent_source_contract_test.go"): str(test)}}))
        subprocess.run(["go", "test", "-count=1", "-v", "-overlay", str(overlay), "./cmd/gc", "-run", "^TestBdParentGrammarAgainstSource$"], cwd=root, check=True)
        print(f"PASS: {len(expected)} dependency spellings compared against {beads}")


if __name__ == "__main__":
    main()
