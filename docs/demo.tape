# Terminal demo for the README. Regenerate with:  make pg && make demo
# Records the real CLI against a real PostgreSQL 17; nothing is staged.
# Before recording: ./bin/migrate-safe seed --dsn postgres://postgres:pg@localhost:55432/postgres --db shop --rows 2000000

Output docs/demo.gif

Set Shell "bash"
Set FontSize 14
Set Width 1420
Set Height 540
Set Padding 18
Set Theme "GitHub Dark"
Set TypingSpeed 30ms

Hide
Type "export PATH=/vhs/bin:$PATH PS1='$ ' DSN=postgres://postgres:pg@localhost:55432/shop && cd /vhs && clear"
Enter
Show

Type "cat examples/migrations/01-create-index/unsafe.sql"
Enter
Sleep 1500ms

Type "migrate-safe lint examples/migrations/01-create-index/unsafe.sql"
Enter
Sleep 5s

Type "clear"
Enter
Sleep 300ms

Type "migrate-safe measure -q --dsn $DSN examples/migrations/01-create-index/unsafe.sql   # 2M orders, live traffic"
Enter
Sleep 14s

Type "clear"
Enter
Sleep 300ms

Type "migrate-safe measure -q --dsn $DSN examples/migrations/01-create-index/safe.sql"
Enter
Sleep 16s
