# RunPod fixtures

These are pinned external inputs. A test maps each one and compares against an
expected ember value, so an upstream API change surfaces here as a test diff
rather than at runtime with a GPU attached.

## Where they came from, and what that means

They were built from RunPod's **published OpenAPI schema** for
`https://api.runpod.io/v2` — field names, types, enum values and the documented
examples — and not captured from a live account.

That distinction is worth stating plainly, because it is the limit of what
these fixtures prove:

- Field **names, shapes and enum values** are as authoritative as the schema
  is. Those are what `types.go` and `map.go` are written against.
- Field **combinations** are ember's reading of the documentation, not an
  observation. The schema says `runtime` and `ssh.direct` are null when
  listing pods and until a public `22/tcp` mapping is assigned; the fixtures
  encode that, but nobody has watched it happen.
- Anything the schema does not describe is absent here. A real response may
  carry fields these do not.

So the mapping is tested, and the *assumption that these are what RunPod
actually sends* is not. Replace each file with a captured live response as soon
as an account exists, keeping the expectations: a fixture that changes shape
when replaced is exactly the finding worth having, and it is cheaper to learn
it from a test diff than from a pod that provisioned and never became ready.

## Files

| file | what it is |
|---|---|
| `pod_running.json` | a ready pod: RUNNING, `ssh.direct` assigned, one public `22/tcp` |
| `pod_provisioning.json` | PROVISIONING, no runtime, no ssh |
| `pod_running_no_ssh.json` | RUNNING but no `ssh.direct` yet — the gap between status and reachability |
| `pod_exposed_http.json` | a pod with a public `8188/http` port, which must fail the exposure assertion |
| `pod_exited.json` | EXITED, which ember reads as failed |
| `pod_unknown_status.json` | a status ember does not recognise, which must be an error |
| `pods_list.json` | a list response: no runtime, no ssh, plus one pod that is not ember's |
| `gpus.json` | the GPU catalogue, including entries ember must filter out |
