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

Để xem hướng dẫn từng bước, hãy đọc [docs/guide/vi/getting-started.html](docs/guide/vi/getting-started.html); các mục bên dưới giải thích từng phần một cách thủ công.

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

## Phát triển và kiểm thử

```bash
make check            # the gate: gofmt, go vet, staticcheck, unit tests
make test             # unit tests only
make test-integration # everything, including tests that need PostgreSQL
make test-e2e         # the real OpenCode, end to end (slow: minutes)
```

`make check` chạy được trên máy không có cơ sở dữ liệu: các test tích hợp tự bỏ qua trừ khi có đặt `TEST_DATABASE_URL`, nên một lần thất bại luôn là lỗi thật chứ không phải do thiếu môi trường.

Để chạy các test cơ sở dữ liệu:

```bash
make db-up
make test-db-create        # creates the aidev_test database
make test-integration
```

Phân tích tĩnh tùy chọn nhưng nên dùng:

```bash
go install honnef.co/go/tools/cmd/staticcheck@latest
```

`make lint` dùng công cụ đó khi có sẵn và sẽ nói rõ khi chưa có.

Các mục tiêu hữu ích khác: `make db-reset` (xóa dữ liệu và bắt đầu sạch), `make db-logs`, `make help`.

### Những gì các test giữ

Bộ test không chỉ để đo độ phủ; một số test tồn tại để giữ các bất biến cụ thể:

- `VERIFYING` là trạng thái duy nhất có thể tới được `SUCCEEDED`.
- Mọi trạng thái chưa kết thúc đều tới được `CANCELLED`, nên không task nào là không thể dừng.
- Các kiểu liệt kê trong Go và các ràng buộc `CHECK` của SQL không thể lệch nhau — điều này được kiểm chứng bằng cách xác nhận test sẽ thất bại khi một giá trị bị gỡ khỏi ràng buộc.
- `UPDATE` trên nhật ký event bị cơ sở dữ liệu từ chối; xóa một task vẫn xóa theo toàn bộ lịch sử của nó.
- Một task và event tạo ra nó được commit cùng nhau hoặc không commit gì cả.
- Một agent không thể tạo ra một task thành công chỉ bằng cách tuyên bố thành công.
- Một tập tin được ghi trong worktree không hiện ra trong kho mã, kho mã vẫn sạch.
- Việc thu thập diff lộ ra các tập tin mới và không stage bất cứ gì trong worktree.
- Worktree của lần thử thất bại được giữ lại; công việc của lần thành công được commit trước.
- Hủy một task giữa chừng vẫn ghi nhận việc hủy, thay vì để hàng đó kẹt ở `RUNNING`.
- Việc hủy diệt toàn bộ nhóm tiến trình, nên các tiến trình con của lệnh verification không sống sót sau nó.

Một số điều trong đó đã được xác nhận bằng cách cố tình phá hỏng phần cài đặt rồi kiểm tra rằng test thất bại, thay vì cho rằng test xanh là đã có bảo đảm thật.

## Xem một task đã làm gì (tùy chọn)

aidev xuất traces OpenTelemetry khi đã cấu hình đầu OTLP, và không xuất gì cả khi chưa cấu hình. Mỗi lần chạy task có một trace, trong đó lần gọi agent mang theo mức dùng token của nó.

```bash
make jaeger-up   # one container, UI on :16686
make jaeger-env  # prints the tracing object to paste into the conf.json that AIDEV_CONFIG names
aidev task run TASK-000001
```

Để có góc nhìn hướng LLM kèm chi phí, Langfuse tự host cũng dùng được — gồm sáu dịch vụ, nên nó được khởi động riêng:

```bash
make langfuse-up   # six services, UI on :3000
make langfuse-env  # prints the tracing object to paste into the conf.json that AIDEV_CONFIG names
make langfuse-credentials   # the bootstrapped UI login, on :3000
```

Một lần chạy thật trông như thế này:

```text
aidev.task.run            10.97s
  aidev.worktree.create    0.02s
  aidev.agent.run         10.86s   usage {input: 9580, output: 457}
  aidev.verification       0.08s
    aidev.verification.step   0s   go test ./... → exit 0
```

Nó cũng cho thấy thời gian đi vào đâu: agent chiếm 10,86 trên 10,97 giây.

Hai ghi chú rút ra khi làm cho việc này chạy. Langfuse v4 đã bỏ `GET /api/public/traces` — hãy dùng `GET /api/public/v2/observations`, và hãy kiểm tra trạng thái HTTP thay vì đọc trường `data` rỗng trong thân lỗi. Và aidev cố ý **không** truyền cấu hình OTLP của mình cho agent: OpenCode cũng đã được gắn đo, và một lần chạy đã từng đẩy 1536 span nội bộ của chính nó vào backend của bạn dưới tên aidev.

## Khi có sự cố

**Hãy bắt đầu với `aidev doctor`.** Lệnh này kiểm tra, theo thứ tự, rằng cấu hình đọc được, rằng git và agent đã cấu hình đã được cài, rằng cơ sở dữ liệu trả lời và đã được migration, và rằng `workspace_root` ghi được. Mỗi vấn đề đi kèm cách xử lý, mật khẩu cơ sở dữ liệu không bao giờ hiện ra, và lệnh thoát với mã 1 khi có gì đó hỏng. `aidev doctor --json` in cùng kết quả dưới dạng danh sách JSON; skill `aidev:doctor` của plugin đọc kết quả đó và sửa những gì nó sửa an toàn được.

```bash
aidev doctor
# ok    config      Configuration is readable.
# ...
# FAIL  database    Cannot reach the database: ... connection refused.
#       fix: Start PostgreSQL with `make db-up` in the aidev repository and ...
```

**aidev bị tắt giữa lúc một task đang chạy.** Task kẹt ở `RUNNING`, và không có gì nhận lại nó nữa. Hãy hủy nó:

```bash
aidev task list --status RUNNING,VERIFYING
aidev task cancel TASK-000001 --reason "aidev was killed mid-run"
```

Công việc dở dang được giữ lại. aidev cố ý không tự hết hạn một task `RUNNING` cũ — xem [lý do](docs/architecture.md#when-a-run-is-interrupted).

**Không gian làm việc đầy dần.** Các task thất bại cố ý giữ lại worktree của chúng:

```bash
aidev worktree list                        # with tasks, statuses and sizes
aidev worktree remove TASK-000001          # refused if work is uncommitted
aidev worktree remove TASK-000001 --force  # discard it deliberately
```

**Có các volume Docker mang tên task.** Một agent đã chạy `docker compose` bên trong worktree của nó, nơi có một bản sao của `docker-compose.yml`, và Compose đã đặt tên dự án theo thư mục. Chúng chỉ là rác rỗng chứ không phải dữ liệu: `docker volume prune` xóa chúng. Tên dự án cố ý để không ghim — xem [lý do](docs/architecture.md#postgresql-data-and-why-the-compose-project-name-is-not-pinned).

**Cơ sở dữ liệu có được giữ lâu dài không?** Có: nhờ một volume đã đặt tên. `make db-up` giữ nó trong `aidev_aidev-pgdata` (Compose thêm tiền tố tên dự án), `aidev setup` giữ trong `aidev-pgdata`. `make db-down` giữ lại nó và chỉ `make db-reset` mới phá hủy nó. Nếu `aidev setup` phải tạo container của nó trong khi các volume của Compose đã tồn tại, nó sẽ dừng lại và liệt kê chúng thay vì khởi động trên một cơ sở dữ liệu rỗng: hãy truyền `--postgres-volume aidev_aidev-pgdata` để giữ dữ liệu của `make db-up`. Không có PersistentVolumeClaim vì không có Kubernetes.

**Một task thất bại và bạn muốn biết vì sao.**

```bash
aidev task result TASK-000001 --logs   # verification output, agent transcript, diff
aidev task events TASK-000001          # what happened, in order
```

**Một task chạy chậm.** Hầu hết thời gian đó là do mô hình, không phải aidev — đã đo được 0,2 giây việc của chính aidev so với 9 tới 656 giây của agent. Hãy xem `aidev task events` để biết nó đang ở giai đoạn nào, và cân nhắc một mô hình nhanh hơn.

## Tài liệu

| | |
|---|---|
| **[docs/guide/vi/index.html](docs/guide/vi/index.html)** | **Hãy bắt đầu ở đây nếu bạn là người mới.** Hướng dẫn bốn trang viết cho tuần đầu tiên: aidev là gì, bắt đầu, gỡ lỗi, và tham khảo đầy đủ. Hãy mở nó trong trình duyệt |
| [docs/research.md](docs/research.md) | Những gì Claude Code, OpenCode, git và PostgreSQL đã cài đặt thật sự làm — đã đo đạc, với các giả định và câu hỏi mở được đánh dấu |
| [docs/architecture.md](docs/architecture.md) | Cách sắp xếp các gói, các quyết định thiết kế và cái giá của chúng, các khe mở rộng |
| [docs/database.md](docs/database.md) | Lược đồ, các ràng buộc, vòng đời, đồng thời, các migration |
| [docs/mcp-tools.md](docs/mcp-tools.md) | Mọi công cụ MCP: đầu vào, đầu ra, lỗi, tác dụng phụ, và những gì cố ý không lộ ra |
| [AGENTS.md](AGENTS.md) | Các quy ước cho agent (và con người) đóng góp vào kho mã này |
