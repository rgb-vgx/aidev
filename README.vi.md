# aidev

*Đây là bản tiếng Việt của [README.md](README.md); các lệnh, đường dẫn và khóa cấu hình được giữ nguyên như bản gốc.*

aidev là một mặt phẳng điều khiển local-first để giao việc triển khai cho một
coding agent và **tự mình kiểm chứng kết quả**.

Một planner — Claude Code, hoặc chính bạn ở terminal — giao cho aidev một task. aidev tạo
một worktree git tách biệt, chạy agent bên trong nó, rồi tự mình chạy các lệnh test
của task và quyết định kết quả dựa trên mã thoát. Ý kiến của agent
về việc nó có thành công hay không không phải là đầu vào của quyết định đó.

Mọi thứ — task, từng lần thử, output đã thu lại, diff, kết
quả verification, cùng toàn bộ lịch sử event — đều được lưu trong PostgreSQL.

> **Trạng thái: Giai đoạn 4 trên 5.** Dùng được từ terminal và từ Claude Code.
> Toàn bộ luồng đã được chạy đầu cuối với OpenCode thật, và máy chủ MCP
> đã được đăng ký và kết nối tới Claude Code đã cài. Giai đoạn 5 là củng cố
> và hoàn thiện tài liệu còn lại. Xem
> [docs/architecture.md](docs/architecture.md#status).

## Vì sao aidev tồn tại

Một agent tự báo cáo thành công của chính nó không phải là căn cứ đáng tin. Thiết kế của aidev
biến điều đó thành cấu trúc bắt buộc chứ không phải lời khuyên:

- **Đường duy nhất tới `SUCCEEDED` phải đi qua `VERIFYING`.** Vòng đời của task không có
  cạnh nào từ "agent đã chạy xong" tới "việc đã đúng", nên lời khẳng định test đã qua
  không thể đưa task tới thành công.
- **Một task không có lệnh verification sẽ bị từ chối ngay khi tạo.** aidev không nhận
  công việc mà nó không thể tự xác lập kết quả.
- **Agent chỉ được viết bên trong một worktree git riêng.** Đây không phải là
  chuyện tiện lợi: qua đo đạc thực tế, OpenCode viết tập tin ở chế độ không tương tác mà không
  hiện câu hỏi cho phép nào, nên worktree là ranh giới cách ly duy nhất còn lại
  ([docs/research.md §2.6](docs/research.md)).
- **Lịch sử chỉ được ghi thêm.** Cơ sở dữ liệu từ chối `UPDATE` trên nhật ký event.

Bộ test tích hợp chứa test nói lên toàn bộ ý tưởng: một agent báo cáo
*"All done! Tests pass."* mà không chạm vào một tập tin nào sẽ tạo ra một task
**FAILED** — lời khẳng định của nó được ghi lại nguyên văn, đặt cạnh kết quả verification
bác bỏ nó.

## Kiến trúc

```text
 Claude Code ──MCP(stdio)──▶ aidev ──▶ OpenCode ──▶ git worktree
  (planner)                    │                     (isolation)
                               │                          │
                               │                          ▼
                               │                   independent verification
                               │                   (aidev runs the commands)
                               ▼
                          PostgreSQL
                     (state + event history)
```

Xem [docs/architecture.md](docs/architecture.md) để biết cách sắp xếp các gói và lý do
đằng sau từng quyết định, và [docs/database.md](docs/database.md) để biết lược đồ
của cơ sở dữ liệu.

## Yêu cầu

| | Phiên bản | Ghi chú |
|---|---|---|
| Go | **≥ 1.25** | MCP SDK đòi hỏi bản này; với Go 1.24 toolchain sẽ tự tải 1.26 |
| Docker + Compose | bản mới bất kỳ | PostgreSQL để phát triển trên máy cá nhân |
| git | ≥ 2.23 | hỗ trợ worktree |
| OpenCode | 1.18+ | chỉ cần từ Giai đoạn 2; đã kiểm chứng với 1.18.30 |
| Claude Code | 2.x | chỉ cần từ Giai đoạn 4; đã kiểm chứng với 2.1.268 |

## Bắt đầu nhanh

Bạn cần Go 1.25 trở lên, git, OpenCode và, trừ khi đã có sẵn PostgreSQL, Docker.

Cài đặt chương trình:

```bash
git clone <this repository> aidev
cd aidev
make install
```

Chuẩn bị aidev bằng một lệnh duy nhất:

```bash
aidev setup
```

Lệnh này khởi động PostgreSQL trong Docker (container `aidev-postgres` trên `127.0.0.1:5434`), ghi `~/.config/aidev/conf.json` (riêng tư, không bao giờ bị ghi đè), áp lược đồ, rồi in ra những gì cần thêm tiếp theo. Chạy lại cũng an toàn, ví dụ sau khi khởi động lại máy.

Nếu công ty bạn lấy image từ một registry riêng, hãy trỏ setup tới đó:

```bash
aidev setup --postgres-image registry.example.com/library/postgres:16-alpine
```

Nếu bạn đã có sẵn PostgreSQL, hãy bỏ qua Docker hoàn toàn:

```bash
aidev setup --database-url 'postgres://user:password@host:5432/aidev'
```

Trỏ `AIDEV_CONFIG` tới tập tin đó, như setup đã in ra:

```bash
export AIDEV_CONFIG="$HOME/.config/aidev/conf.json"   # add to ~/.bashrc or ~/.zshrc
```

Với Claude Code, hãy thêm `"AIDEV_CONFIG"` vào đối tượng `"env"` trong `~/.claude/settings.json` (setup sẽ in ra dòng chính xác).

Kiểm tra mọi thứ:

```bash
aidev doctor
```

Mọi dòng đều phải báo `ok`; mỗi lỗi đều nói rõ cách sửa.

Dùng aidev từ Claude Code:

```bash
claude plugin marketplace add /abs/path/to/aidev
claude plugin install aidev@aidev
```

Rồi nhờ Claude "giao việc này cho aidev"; các skill của plugin sẽ lo việc ủy thác, đánh giá và sửa lỗi (`aidev:delegate`, `aidev:review`, `aidev:doctor`).

Để xem hướng dẫn từng bước, hãy đọc [docs/guide/getting-started.html](docs/guide/vi/getting-started.html); các mục bên dưới giải thích từng phần một cách thủ công.

## Cài đặt

```bash
git clone <this repository> aidev
cd aidev
make build            # produces bin/aidev
```

Hoặc cài đặt vào `PATH` của bạn:

```bash
make install          # go install ./cmd/aidev
```

## Thiết lập PostgreSQL

```bash
make db-up            # starts PostgreSQL and waits until it is healthy
```

Lệnh này công bố PostgreSQL trên **127.0.0.1:5434**, chứ không phải 5432 như thường lệ. Trên máy nơi aidev được phát triển, 5432 đã là dịch vụ PostgreSQL của máy chủ còn 5433 là container của một dự án khác, nên giá trị mặc định thông thường đã khiến lần `docker compose up` đầu tiên thất bại. Hãy ghi đè cổng nếu 5434 cũng đã bị chiếm:

```bash
AIDEV_DB_PORT=5440 make db-up
```

Sau đó cấu hình aidev và áp dụng lược đồ:

```bash
make install                                            # puts aidev on your PATH
mkdir -p ~/.config/aidev
cp conf/conf.example.json ~/.config/aidev/conf.json     # then edit if you changed the port
export AIDEV_CONFIG="$HOME/.config/aidev/conf.json"     # put this line in your shell profile
aidev migrate
```

aidev chỉ đọc đúng tập tin mà `AIDEV_CONFIG` chỉ tới, vì vậy lệnh export phải có mặt trong mọi terminal mới. Hãy đặt dòng đó vào hồ sơ shell của bạn (`~/.bashrc`, `~/.zshrc`, hoặc tập tin tương đương của shell bạn dùng) và đây là việc thiết lập một lần duy nhất. `aidev config` sẽ in ra nó đã dùng tập tin nào.

Một conf.json chứa mật khẩu cơ sở dữ liệu, vì vậy hãy giữ nó ngoài git. Hãy sao chép nó tới `conf/conf.json` nếu bạn thích — đường dẫn đó vốn đã bị bỏ qua — hoặc đơn giản là đừng bao giờ commit đường dẫn mà bạn dùng. `aidev config` che các bí mật, nên đọc lại cũng an toàn.

`aidev migrate` có tính lũy đẳng — hãy chạy lại tùy thích. Các migration được nhúng trong chương trình; không cần cài thêm công cụ migration riêng nào.

## Cấu hình

aidev chỉ đọc cấu hình từ một tập tin JSON duy nhất: conf.json mà `AIDEV_CONFIG` chỉ tới. Không có gì khác trong môi trường được dùng để cấu hình — một biến còn sót trong hồ sơ shell không được âm thầm lấn át tập tin mà người dùng đang đọc và sửa.

Mọi thiết lập đều tùy chọn, trừ `database.url`. Các giá trị mặc định bên dưới là những gì chương trình dùng khi thiếu khóa; conf/conf.example.json trình bày tất cả trong một tập tin.

| Thiết lập | Mặc định | Mục đích |
|---|---|---|
| `database.url` | *(required)* | chuỗi kết nối PostgreSQL |
| `workspace_root` | `~/.local/share/aidev/worktrees` | nơi tạo các worktree của task; mọi đường dẫn worktree đều phải nằm bên trong nó |
| `tasks.timeout` | `30m` | giới hạn một lần chạy agent khi task không tự đặt giá trị riêng |
| `tasks.verification_timeout` | `10m` | giới hạn một bước verification |
| `tasks.max_output_bytes` | `1048576` | giới hạn thu output cho mỗi luồng; phần output vượt quá sẽ bị bỏ và đánh dấu đã cắt ngắn |
| `tasks.worktree_cleanup` | `on-success` | `on-success` commit công việc rồi xóa worktree; `never` giữ lại mọi worktree. Không chế độ nào xóa công việc đã thất bại |
| `agent.backend` | `opencode` | implementation agent nào chạy các task: `opencode` hoặc `codex` |
| `agent.opencode.command` | `opencode` | chương trình thực thi OpenCode |
| `agent.opencode.model` | `opencode/muse-spark-1.3-contributor-free` | không cần chứng thực; đặt thành chuỗi rỗng để OpenCode tự chọn |
| `agent.opencode.agent` | `build` | agent OpenCode mặc định |
| `agent.codex.command` | `codex` | chương trình thực thi Codex |
| `agent.codex.profile` | *(none)* | được truyền thành `--profile` khi có đặt |
| `agent.codex.model` | *(none)* | được truyền thành `-m` khi có đặt; để trống thì Codex tự chọn |
| `agent.codex.sandbox` | `workspace-write` | được truyền thành `--sandbox` khi có đặt |
| `agent.routing` | *(none)* | một đối tượng ánh xạ độ khó của task (`TRIVIAL`, `STANDARD`, `HARD`) tới mô hình xứng đáng với độ khó đó; độ khó nào không có mục sẽ để backend tự chọn |
| `log_level` | `info` | `debug`, `info`, `warn` hoặc `error` |
| `tracing.endpoint` | *(none)* | URL HTTP OTLP cơ sở; không bật tracing khi chưa đặt gì |
| `tracing.traces_endpoint` | *(none)* | URL đầy đủ mà bộ xuất traces gửi tới, được ưu tiên hơn `tracing.endpoint` |
| `tracing.headers` | *(none)* | một đối tượng chứa các header OTLP bổ sung, chẳng hạn `Authorization` |
| `tracing.service_name` | `aidev` | tên dịch vụ mà các span mang theo |
| `tracing.sample_ratio` | `1` | tỉ lệ traces mới được lấy mẫu, từ 0 tới 1 |

`agent.routing` và `tracing.headers` là các đối tượng, không phải giá trị đơn: các mục của chúng là giá trị bạn tự viết, không phải thiết lập con.

Giá trị mặc định cho thời gian chờ của task được cố ý để rộng rãi: lần chạy đầu tiên của OpenCode trên một kho mã chưa từng gặp đã được đo mất hơn bốn phút mới cho ra output đầu tiên, rồi sau đó chỉ còn vài giây. Một giá trị mặc định ngắn sẽ khiến task đầu tiên của mọi lập trình viên mới đều thất bại theo cách trông như lỗi của aidev.

Để xem chính xác aidev đã chốt những gì — với mật khẩu cơ sở dữ liệu đã được che:

```bash
aidev config
aidev config --json
```

## Task đầu tiên của bạn

Khi PostgreSQL đã chạy và lược đồ đã được áp dụng, từ bên trong bất kỳ kho git nào:

```bash
aidev task create \
  --title "Add a Greet function" \
  --description "Create greet.go with Greet(name string) string returning \"Hello, \" + name" \
  --verify 'go test ./...' \
  --verify 'go vet ./...'
# created TASK-000001  Add a Greet function
#   run it with: aidev task run TASK-000001

aidev task run TASK-000001
```

Việc này mất vài phút chứ không phải vài giây — phần lớn thời gian là OpenCode chạy. Trong khi chạy, nhật ký event cho thấy nó đang ở đâu:

```bash
aidev task events TASK-000001
#   1  task.created
#   2  task.ready
#   3  task.started
#   4  task.worktree_created
#   5  task.worker_started
```

Một lần chạy thật của đúng ví dụ này, trên một kho mã mà test không biên dịch được cho tới khi công việc hoàn tất:

```text
TASK-000001 succeeded: 2/2 verification steps passed, work committed on aidev/TASK-000001

agent (opencode)
  outcome       SUCCEEDED
  changed files 1
  took          10m56s
  says          Tests: The=": chúng, DP eng 들어 ஆக ree? (embцион upcoming…

verification (run by aidev)
  ✓ PASSED    go test ./...
  ✓ PASSED    go vet ./...

worktree
  REMOVED  /home/you/.local/share/aidev/worktrees/TASK-000001-a1
  branch  aidev/TASK-000001
```

Hãy chú ý dòng `says`. Lần chạy đó dùng một mô hình miễn phí mà tóm tắt cuối của nó khá lộn xộn — nhưng điều đó không quan trọng. Đoạn mã nó viết ra đúng, và điều khẳng định điều đó là aidev đã tự chạy `go test` và `go vet`. Lời tường thuật của agent về công việc của chính nó được ghi lại vì có ích cho việc chẩn đoán, chứ không bao giờ là bằng chứng.

Mặt còn lại của cùng một đồng xu, với một agent báo cáo thành công mà không chạm vào gì:

```text
TASK-000002 FAILED (VERIFICATION): verification did not pass: 0/1 steps passed

agent (opencode)
  outcome       SUCCEEDED
  changed files 0
  says          Done! I implemented the function and all tests pass.

verification (run by aidev)
  ✗ FAILED    test -f farewell.go  (exit 1)

the work was kept for inspection
  cd /home/you/.local/share/aidev/worktrees/TASK-000002-a1
```

`aidev task run` thoát với mã khác không khi một task không thành công, nên `aidev task run TASK-000001 && ./deploy.sh` sẽ hành xử đúng như bạn mong đợi.
