Output "/tmp/sshmgr-tutorial-frame.mp4"
Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 640
Hide
Type "source ./.demo-env" Sleep 200ms Enter
Sleep 500ms
Type "clear" Sleep 200ms Enter
Sleep 500ms
Show
Type "./sshmgr tui" Sleep 300ms Enter
Sleep 4s
Hide
Enter
Sleep 2s
Type "demo-gpu" Sleep 200ms Enter
Type "127.0.0.1" Sleep 200ms Enter
Type "deploy" Sleep 200ms Enter
Ctrl+U
Type "2222" Sleep 800ms Enter
Sleep 1s
Show
Type "deploy-password" Sleep 500ms
Screenshot "../05-server-password.png"
Sleep 1s
