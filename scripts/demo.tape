# vhs tape for the README demo GIF (https://github.com/charmbracelet/vhs).
# Regenerate from the repo root:
#   make build && make fleet-up
#   source .tmp/fleet/env.sh && ./bin/awsmux targets >/dev/null   # pre-warm the identity cache
#   vhs scripts/demo.tape
# Then shrink the result (roughly 6.6 MB down to 4.4 MB, text stays crisp):
#   ffmpeg -i docs/demo.gif -filter_complex \
#     "fps=12,split[a][b];[a]palettegen=max_colors=64:stats_mode=diff[p];[b][p]paletteuse=dither=none" \
#     docs/demo-small.gif && mv docs/demo-small.gif docs/demo.gif

Output docs/demo.gif

# Layout note: GitHub scales the GIF to the README column, so on-screen text
# size is set by the terminal's column count, not FontSize. These dimensions
# give ~130 columns (vhs glyph cells are ~0.66em wide): enough for the targets
# table, the plan detail rows, and the 127-char approve/apply "Next" line to
# stay unwrapped. Long jsonl lines wrap during the fast scroll, which is fine.
Set Shell bash
Set FontSize 20
Set Width 1760
Set Height 900
Set Padding 20
Set Framerate 15
Set TypingSpeed 25ms
Set Theme "Catppuccin Mocha"
Set WindowBar Colorful
Set WaitTimeout 90s

Hide
Type "source .tmp/fleet/env.sh && export PATH=$PWD/bin:$PATH && clear"
Enter
Wait
Show

Type "# discover targets, every identity verified with STS before anything runs"
Enter
Type "awsmux targets --profiles 'payments-*'"
Enter
Wait
Sleep 3s

Type "# one read-only command, all 100 accounts in parallel"
Enter
Type "awsmux run --dedupe --format jsonl -- ec2 describe-vpcs --query 'Vpcs[].VpcId'"
Enter
Wait
Sleep 3.5s

Type "clear"
Enter
Wait

Type "# destructive commands are refused outright"
Enter
Type "awsmux run --profiles payments-prod-1 -- ec2 revoke-security-group-ingress --group-name legacy-bastion --protocol tcp --port 22 --cidr 0.0.0.0/0"
Enter
Wait
Sleep 2.5s

Type "# you get an immutable, hash-bound plan for a human to approve instead"
Enter
Type "awsmux plan --profiles payments-prod-1 -- ec2 revoke-security-group-ingress --group-name legacy-bastion --protocol tcp --port 22 --cidr 0.0.0.0/0"
Enter
Wait
Sleep 7s
