# VHS tape for the README demo — regenerate with: vhs demo/demo.tape
# Requires: bin/firedrill built, examples/demo.dump generated, Docker running,
# a signing key (firedrill keygen). Run from the repo root.
Output demo/demo.gif

Set FontSize 15
Set Width 900
Set Height 560
Set Theme "Catppuccin Mocha"
Set Padding 12
Set TypingSpeed 40ms

Type "firedrill run payments-db -f examples/firedrill.yaml"
Sleep 500ms
Enter
Sleep 14s
Type "firedrill verify-evidence evidence/payments-db-*.json"
Sleep 500ms
Enter
Sleep 3s
