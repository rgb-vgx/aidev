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

## Các lệnh

```bash
aidev version
aidev setup [--config PATH] [--database-url URL] [--postgres-image IMAGE]
            [--postgres-port N] [--postgres-volume NAME]
            [--workspace-root DIR]                        # prepare a new machine
aidev doctor [--json]                 # check that aidev can work, and how to fix it
aidev config [--json]                 # the resolved configuration, password redacted
aidev migrate [--json]                # apply pending migrations

aidev task create --title T --verify CMD [--repo .] [--description D]
                  [--acceptance A] [--agent build] [--priority N]
                  [--requires-approval] [--base-ref REF] [--timeout 30m]
aidev task list   [--status S,S] [--repo .] [--limit N] [--json]
aidev task get    <task> [--json]
aidev task run    <task> [--json]
aidev task result <task> [--logs] [--json]
aidev task events <task> [--payload] [--after SEQ] [--json]
aidev task cancel <task> [--reason R] [--json]
aidev task approve <task> [--deny] [--by WHO] [--reason R] [--json]
```

`<task>` là mã tham chiếu (`TASK-000001`, không phân biệt chữ hoa chữ thường) hoặc UUID.
Mọi lệnh trừ `setup` đều nhận `--json` để in kết quả cho máy đọc; dạng cho người đọc là
mặc định. Nhật ký luôn ghi ra stderr, nên `aidev task get TASK-000001 --json | jq`
vẫn chạy được trong khi các chẩn đoán hiển thị bình thường.

Các task bị đánh dấu `--requires-approval` sẽ dừng lại trước khi làm bất cứ việc gì — không worktree, không
lần thử nào — cho tới khi `aidev task approve` cho phép chúng tiếp tục.

## Chuyện gì xảy ra bên trong

```text
create task ──▶ isolate in a git worktree ──▶ run the agent there
                                                      │
  record in PostgreSQL ◀── verify independently ◀──────┘
                                   │
                    passed ──▶ commit to aidev/<ref>, remove the worktree
                    failed ──▶ keep the worktree for inspection
```

Một task thành công để lại một commit trên nhánh riêng của nó, nên kết quả xem lại được
bằng git thông thường:

```bash
git log --oneline aidev/TASK-000001
git diff main..aidev/TASK-000001
```

Không có gì được hợp nhất, và không có gì bao giờ được commit lên nhánh làm việc của bạn.

Một task thất bại để lại worktree của nó nguyên vẹn đúng như agent đã bỏ lại, nằm dưới
`workspace_root`, vì công việc dở dang thường là thứ hữu ích nhất của một thất bại.
`git worktree remove` từ chối xóa công việc chưa commit và aidev không bao giờ
tự ý vượt qua điều đó, nên điều này đúng bất kể cấu hình ra sao. Xem
[chính sách dọn dẹp](docs/architecture.md#cleanup-policy).

Output cho người đọc đi ra **stdout**; log có cấu trúc đi ra **stderr**. Sự phân tách
này mang tính quyết định: khi aidev chạy như một máy chủ MCP, stdout mang giao thức
JSON-RPC, nên không có gì khác được phép ghi ra đó.

## Thiết lập OpenCode

Hãy cài OpenCode sao cho gọi được trên `PATH`, hoặc đặt `agent.opencode.command`. Không
cần chứng thực API nào: các mô hình công khai `opencode/*` chạy được và báo chi phí bằng
không, và đó là cách test đầu cuối của dự án này chạy mà không cần khóa.

Mô hình mặc định là `opencode/muse-spark-1.3-contributor-free`, được chọn bằng
đo đạc chứ không phải theo tên. Trên cùng những prompt đơn giản giống hệt nhau, nó trả lời trong
3.2–3.9s qua các lần chạy lặp lại; mô hình miễn phí còn lại dao động giữa 3.9s và 100.5s
cho cùng công việc — tức là khác biệt giữa một task một phút và một
task mười một phút. Hãy đặt `agent.opencode.model` thành bất kỳ mô hình nào bạn có chứng thực, hoặc
thành chuỗi rỗng để OpenCode tự quyết.

```bash
opencode --version          # verified against 1.18.30
opencode models | head      # the opencode/* entries need no credentials
```

aidev gọi `opencode run --dir <worktree> --format json -- <prompt>` rồi đọc
luồng event phân tách bằng xuống dòng. Bạn không bao giờ tự gõ lệnh đó; biết nó là việc của
aidev, không phải của planner.

Hai hành vi đã đo đạc đáng biết trước task đầu tiên của bạn:

- **Một task lâu đúng bằng thời gian mô hình chạy.** Phần việc của chính aidev trong một lần chạy —
  worktree, diff, verification, mọi lần ghi cơ sở dữ liệu — được đo chỉ 0.2 giây
  so với thời gian agent từ 9 tới 656 giây. Độ trễ của bậc miễn phí là biến số quyết định,
  và không phải lúc nào cũng đoán trước được, đó là lý do
  `tasks.timeout` mặc định 30 phút.
- **OpenCode ghi tập tin mà không hỏi**, ngay cả khi không có cờ `--auto`. Đó
  là lý do mọi task chạy trong một worktree riêng và aidev từ chối đường dẫn worktree
  nằm bên trong kho mã của bạn.

Xem [docs/research.md](docs/research.md) để biết các số đo đằng sau tất cả những điều này.

## Thiết lập MCP cho Claude Code

Đây chính là lý do aidev tồn tại: Claude Code lập kế hoạch và đánh giá, aidev cách ly, chạy và
kiểm chứng.

```bash
make install                                          # aidev on your PATH
claude mcp add --scope user -e AIDEV_CONFIG=/abs/path/conf.json aidev -- /abs/path/aidev mcp

claude mcp list
# aidev: /abs/path/aidev mcp - ✔ Connected
```

**Hãy dùng `--scope user`.** Lệnh này đăng ký aidev cho mọi dự án, và đó đúng là ý nghĩa của nó:
bạn mở Claude Code trong bất kỳ kho mã nào mình đang làm việc rồi ủy thác ngay tại đó.
`--scope local` giam nó trong một thư mục dự án duy nhất, còn `--scope project`
ghi một tập tin `.mcp.json` dùng chung mà mỗi người phải duyệt một lần.

Không có chứng thực nào nằm trên lệnh đăng ký: `AIDEV_CONFIG` chỉ tới conf.json, và
aidev đọc mật khẩu cơ sở dữ liệu từ đó, nên `~/.claude.json` không chứa
chuỗi kết nối nào. Hãy dùng đúng tập tin và đường dẫn mà `aidev config` báo, để máy chủ
khởi động đã có sẵn cấu hình. Tám công cụ sau đây sẽ khả dụng:

| Công cụ | Mục đích |
|---|---|
| `aidev_create_task` | tạo task; bắt buộc phải có lệnh verification |
| `aidev_run_task` | cách ly, ủy thác, kiểm chứng, ghi nhận |
| `aidev_get_task_result` | kết quả, kèm bằng chứng verification |
| `aidev_get_task_events` | lịch sử, và tiến độ khi task đang chạy |
| `aidev_list_tasks` / `aidev_get_task` | tìm công việc |
| `aidev_cancel_task` | dừng task; worktree của nó được giữ lại |
| `aidev_approve_task` | con người cho phép task bị chặn chạy tiếp |

Một lần chạy mất vài phút, nên `aidev_run_task` chờ một khoảng thời gian giới hạn rồi trả về với
`still_running: true` trong khi task vẫn tiếp tục; planner thăm dò
`aidev_get_task_result`. Trường cần đọc là `succeeded`, chỉ đúng khi
verification của chính aidev đã qua.

### Hoặc cài plugin Claude Code

Kho mã này cũng là một chợ plugin. plugin `aidev` đăng ký cùng một
máy chủ MCP và thêm hai skill dạy Claude quy trình xung quanh nó:
`aidev:delegate` (thống nhất thế nào là "xong", viết test lỗi trên nhánh `spec/*`,
giữ các task nhỏ, ủy thác, khôi phục sau một task thất bại) và `aidev:review` (soát
diff vượt ra ngoài các test xanh, hợp nhất, báo cáo theo cách nói của người dùng).

```bash
make install                                   # the plugin runs `aidev mcp` from PATH
export AIDEV_CONFIG="$PWD/conf/conf.json"      # in your shell profile; the plugin passes it on
claude plugin marketplace add /abs/path/to/aidev
claude plugin install aidev@aidev

claude mcp list
# plugin:aidev:aidev: aidev mcp - ✔ Connected
```

Máy chủ của plugin đọc `AIDEV_CONFIG` từ môi trường nơi Claude Code khởi động,
nên hãy export nó trong hồ sơ shell của bạn. Chỉ dùng hoặc plugin hoặc lệnh đăng ký `claude mcp
add` ở trên, đừng dùng cả hai: dùng cả hai thì mọi công cụ bị liệt kê hai lần.

Hai điều cần biết trước khi ủy thác việc trong một kho mã thật.

**Hãy nói cho agent biết lệnh verification nào phù hợp.** Trong monorepo, `go test ./...`
từ gốc hiếm khi là kiểm tra đúng. Hãy nêu đúng cái bao phủ thay đổi:
`--verify 'go test ./backend/...'`, hoặc lệnh test riêng của một dịch vụ.

**Worktree là một bản checkout sạch: chỉ gồm các tập tin đã được theo dõi.** Các phụ thuộc nằm
ngoài git không có ở đó. `go test` vẫn ổn, vì module cache được dùng chung,
nhưng `npm test` hoặc `pytest` sẽ thất bại trong một worktree mới trừ khi verification
của task cài đặt những gì nó cần trước — ví dụ
`--verify 'npm --prefix web ci'` trước `--verify 'npm --prefix web test'`.

Đầy đủ lược đồ, lỗi và tác dụng phụ: **[docs/mcp-tools.md](docs/mcp-tools.md)**.

### Việc này đã được chạy thật, không chỉ đấu nối xong

Claude Code được giao một kho mã có test không biên dịch được, được dặn chỉ dùng
các công cụ MCP của aidev, và bị từ chối rõ ràng `Edit`, `Write`, `Read` và `Bash` —
nên công việc không thể đến từ đâu khác ngoài aidev. Nó đã tạo và chạy
task, rồi báo cáo lại:

```text
The task is TASK-000008 and it succeeded (status SUCCEEDED, first attempt, about 9 seconds).

| Command         | Exit code | Result                        |
| go test ./...   | 0         | PASSED: ok  demo (cached)     |
| go vet ./...    | 0         | PASSED, no output             |

What changed: one new file, reverse.go. reverse_test.go was not modified.
Where the work is: committed on branch aidev/TASK-000008 (commit 1aeca30).
Nothing was merged into your branch, and the task's worktree has been removed.
```

Đã kiểm tra lại sau đó mà không tin vào bất cứ điều gì trong đó: task nằm trong PostgreSQL với cả hai
bước verification ở mã thoát 0, cây làm việc của chính kho mã sạch sẽ và
không có reverse.go, còn nhánh kia thì có, và `go clean -testcache && go test` chạy tay
trên nhánh đó cho `TestReverse` qua. Toàn bộ cuộc trao đổi mất 35 giây.
