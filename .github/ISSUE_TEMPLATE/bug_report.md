---
name: Bug report
about: A drill misbehaved, crashed, or produced a wrong verdict
labels: bug
---

**What happened**

**What you expected**

**Drill spec** (redact URIs/credentials — never paste secrets):
```yaml
```

**Output** (`firedrill run … --no-color`):
```
```

**Environment**
- FireDrill version (`firedrill --version`):
- Driver / sandbox provider:
- OS / Docker / Kubernetes versions:

**Severity note**: if the bug caused a drill to report `RECOVERY VERIFIED` when recovery was actually broken (false positive), say so explicitly — those are treated as critical.
