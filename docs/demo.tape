# Regenerate with: vhs docs/demo.tape
# Needs Docker running (the demo boots LocalStack itself); the hidden
# warm-up run seeds the sandbox and warms the identity cache so the
# recorded timings reflect steady state.
Output docs/demo.gif

Set FontSize 15
Set Width 1050
Set Height 640
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms
Set Padding 12

Hide
Type "PATH=$PWD:$PATH && rm -rf ~/.awsmux/demo && awsmux demo targets --dedupe >/dev/null 2>&1 && clear"
Enter
Sleep 30s
Show

Type "awsmux demo targets --profiles 'payments-prod-*'"
Enter
Sleep 4.5s

Type "clear"
Enter

Type "awsmux demo run --dedupe --format jsonl -- ec2 describe-vpcs --query 'Vpcs[].VpcId'"
Enter
Sleep 11s

Type "clear"
Enter

Type "awsmux demo plan --profiles payments-prod-1 -- ec2 revoke-security-group-ingress --group-name legacy-bastion --protocol tcp --port 22 --cidr 0.0.0.0/0"
Enter
Sleep 6.5s
