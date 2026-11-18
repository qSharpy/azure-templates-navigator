#!/bin/bash
# GitHub-backed preview spike. Token only in env; never printed.
set -u
. "$1"   # BASE SNAP BAD
ORG=https://dev.azure.com/bogdanbujor; P=personal; DEF=4
TOK=$(az account get-access-token --resource 499b84ac-1321-427f-aa17-267ca6975798 --query accessToken -o tsv)
H=(-H "Authorization: Bearer $TOK" -H "X-VSS-ForceMsaPassThrough: true" -H "X-TFS-FedAuthRedirect: Suppress" -H "Content-Type: application/json")
api() { curl -s "${H[@]}" "$@"; }
runs() { api "$ORG/$P/_apis/build/builds?definitions=$DEF&api-version=7.1" | python3 -c 'import sys,json;print(json.load(sys.stdin)["count"])'; }
prev() { # name body
  out=$(api -X POST "$ORG/$P/_apis/pipelines/$DEF/preview?api-version=7.1" -d "$2")
  echo "== $1"; echo "$out" | python3 -c '
import sys,json
d=json.load(sys.stdin)
if "finalYaml" in d:
  y=d["finalYaml"]; print("COMPILES; echo lines:", [l.strip() for l in y.splitlines() if "echo" in l])
else: print("ERROR", d.get("typeKey"), d.get("message"))'
}
echo "runs before: $(runs)"
echo "== definitions filtered by GitHub repo"
api "$ORG/$P/_apis/build/definitions?includeAllProperties=true&repositoryType=GitHub&repositoryId=qSharpy/atn-gh-spike&api-version=7.1" | python3 -c '
import sys,json; d=json.load(sys.stdin); print([(x["id"],x["repository"]["type"],x["repository"]["id"],x["process"].get("yamlFilename")) for x in d["value"]])'
echo "== same filter, other-case repo id"
api "$ORG/$P/_apis/build/definitions?includeAllProperties=true&repositoryType=GitHub&repositoryId=qsharpy/ATN-gh-spike&api-version=7.1" | python3 -c '
import sys,json; d=json.load(sys.stdin); print([x["id"] for x in d["value"]])'
echo "== TfsGit filter with the GitHub id (should be empty)"
api "$ORG/$P/_apis/build/definitions?includeAllProperties=true&repositoryType=TfsGit&repositoryId=qSharpy/atn-gh-spike&api-version=7.1" | python3 -c '
import sys,json; d=json.load(sys.stdin); print([x["id"] for x in d["value"]])'
prev "T1 plain" '{"previewRun":true}'
prev "T2 self=custom ref snap" '{"previewRun":true,"resources":{"repositories":{"self":{"refName":"refs/atn/preview/test/snap"}}}}'
prev "T3 self=custom ref snap @version SNAP" "{\"previewRun\":true,\"resources\":{\"repositories\":{\"self\":{\"refName\":\"refs/atn/preview/test/snap\",\"version\":\"$SNAP\"}}}}"
prev "T4 self=custom ref snap @version BASE" "{\"previewRun\":true,\"resources\":{\"repositories\":{\"self\":{\"refName\":\"refs/atn/preview/test/snap\",\"version\":\"$BASE\"}}}}"
prev "T5 self=main @version BASE + yamlOverride" "$(python3 -c '
import json,sys
y=open(sys.argv[2]).read().replace("appName: atn-gh-spike","appName: OVERRIDDEN")
print(json.dumps({"previewRun":True,"yamlOverride":y,"resources":{"repositories":{"self":{"refName":"refs/heads/main","version":sys.argv[1]}}}}))' "$BASE" "$2/azure-pipelines.yml")"
prev "T6 self=main @version SNAP (not on main)" "{\"previewRun\":true,\"resources\":{\"repositories\":{\"self\":{\"refName\":\"refs/heads/main\",\"version\":\"$SNAP\"}}}}"
prev "T7 self=custom ref bad (template error)" '{"previewRun":true,"resources":{"repositories":{"self":{"refName":"refs/atn/preview/test/bad"}}}}'
prev "T8 self=missing custom ref" '{"previewRun":true,"resources":{"repositories":{"self":{"refName":"refs/atn/preview/test/nope"}}}}'
sleep 20
echo "runs after: $(runs)"
