# Regenerate with: vhs docs/demo.tape (records against `awsmux demo`, fully offline)
Output docs/demo.gif

Set FontSize 15
Set Width 1050
Set Height 640
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms
Set Padding 12

Hide
Type "PATH=$PWD:$PATH && rm -rf ~/.awsmux/demo && clear"
Enter
Show

Type "awsmux demo targets"
Enter
Sleep 4.5s

Type "clear"
Enter

Type "awsmux demo run --format jsonl -- lambda list-functions --query 'Functions[].FunctionName'"
Enter
Sleep 5s

Type "clear"
Enter

Type "awsmux demo plan --profiles payments-prod -- ec2 revoke-security-group-ingress --group-id sg-0a1b2c3d --protocol tcp --port 22 --cidr 0.0.0.0/0"
Enter
Sleep 6.5s
