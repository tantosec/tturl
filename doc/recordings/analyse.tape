Output analyse.gif

Set Shell bash
Set FontFamily "Jetbrains Mono"
Set FontSize 20
Set Width 1080
Set Height 700
Set LineHeight 1.05

Set Theme "Catppuccin Mocha"

Set Padding 22
Set Margin 20
Set MarginFill "#11111b"
Set WindowBar Rings
Set BorderRadius 10

Set Framerate 12
Set TypingSpeed 12ms
Set CursorBlink false

Env INPUTRC "/dev/null"
Source transcript-start.tape

Sleep 500ms

Type "# {{RHOST}} is running 'tturl demo-server'"
Enter
Sleep 300ms
Type "# it is a very long way away"
Enter
Enter
Sleep 600ms

Type "ping -c4 -i0.5 {{RHOST}}"
Sleep 600ms
Enter
Wait
Sleep 1s
Enter

Type "# the /sleep route spin-sleeps for a custom duration before returning."
Enter
Sleep 600ms
Type "# we can compare requests whose sleep differs by just 2.5 microseconds."
Enter
Enter
Sleep 600ms

Type "tturl analyse -k \"
Enter
Type "  --connections-fit --batch-rate-max 100/s --connections-fit-max unlimited \"
Enter
Type "  --block 'https://{{RHOST}}:{{RPORT}}/sleep?duration=100us' --name fast \"
Enter
Type "  --block 'https://{{RHOST}}:{{RPORT}}/sleep?duration=102.5us' --name slow"
Sleep 600ms
Enter

Wait
Sleep 2.5s
ScrollUp@16ms 30
Sleep 6s

Source transcript-stop.tape
