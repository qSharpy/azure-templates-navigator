#!/bin/bash
set -u
TBASE=$1 TSNAP=$2
ORG=https://dev.azure.com/bogdanbujor; P=personal; DEF=4
TOK=$(az account get-access-token --resource 499b84ac-1321-427f-aa17-267ca6975798 --query accessToken -o tsv)
H=(-H "Authorization: Bearer $TOK" -H "X-VSS-ForceMsaPassThrough: true" -H "X-TFS-FedAuthRedirect: Suppress" -H "Content-Type: application/json")
api() { curl -s "${H[@]}" "$@"; }
runs() { api "$ORG/$P/_apis/build/builds?definitions=$DEF&api-version=7.1" | python3 -c 'import sys,json;print(json.load(sys.stdin)["count"])'; }
prev() {
  out=$(api -X POST "$ORG/$P/_apis/pipelines/$DEF/preview?api-version=7.1" -d "$2")
  echo "== $1"; echo "$out" | python3 -c '
import sys,json
d=json.load(sys.stdin)
if "finalYaml" in d: print("COMPILES;", [l.strip() for l in d["finalYaml"].splitlines() if "echo" in l])
else: print("ERROR", d.get("typeKey"), d.get("message"))'
}
echo "runs before: $(runs)"
prev "A1 plain" '{"previewRun":true}'
prev "A2 alias=custom ref snap" '{"previewRun":true,"resources":{"repositories":{"templates":{"refName":"refs/atn/preview/test/snap"}}}}'
prev "A3 alias=custom ref snap @TSNAP" "{\"previewRun\":true,\"resources\":{\"repositories\":{\"templates\":{\"refName\":\"refs/atn/preview/test/snap\",\"version\":\"$TSNAP\"}}}}"
prev "A4 alias=custom ref snap @TBASE" "{\"previewRun\":true,\"resources\":{\"repositories\":{\"templates\":{\"refName\":\"refs/atn/preview/test/snap\",\"version\":\"$TBASE\"}}}}"
prev "A5 alias=custom ref bad" '{"previewRun":true,"resources":{"repositories":{"templates":{"refName":"refs/atn/preview/test/bad"}}}}'
prev "A6 alias=missing ref" '{"previewRun":true,"resources":{"repositories":{"templates":{"refName":"refs/atn/preview/test/nope"}}}}'
sleep 20; echo "runs after: $(runs)"
