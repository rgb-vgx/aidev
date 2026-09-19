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
