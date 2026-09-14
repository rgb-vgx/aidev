# OpenSandbox có dùng làm sandbox cho aidev được không?

*Bản tiếng Việt của [opensandbox.md](opensandbox.md). Trích dẫn, lệnh, đường dẫn và khóa cấu hình được giữ nguyên, để người đọc tự mở ra kiểm tra.*

*Trạng thái (15/09/2026): tạm gác. Sandbox là một tính năng tương lai, sẽ xem lại khi task đủ phức tạp để cần tới; hiện không có việc nào trong tài liệu này được lên kế hoạch.*

Câu hỏi: OpenSandbox (https://github.com/opensandbox-group/OpenSandbox) có áp dụng được cho aidev không — để cô lập agent viết code, bước verification, hay cả hai.
Tài liệu này trả lời chỉ dựa trên mã nguồn, tại commit OpenSandbox `d8cfce39dc1d846e580510ca44f44c495cbe95c4`, đối chiếu với mã aidev trong worktree này.

Trả lời ngắn: chưa phải lúc này.
Lỗ hổng mà OpenSandbox sẽ bịt là có thật — agent chạy với tư cách người vận hành, trên máy thật, dùng mạng và credential của người vận hành — nhưng bịt nó theo cách này là đổi một mô hình mối đe dọa nhỏ, kiểm soát được, lấy một mô hình lớn hơn: một server Python mức Alpha nắm socket của Docker daemon, sandbox mặc định dùng chung mạng với máy, image lấy từ những registry mà aidev không dùng ở đâu khác, và một đường verification mà ý nghĩa của exit code không được spec quy định rõ.
Khuyến nghị ở phần cuối là **hoãn**, kèm các điều kiện sẽ làm thay đổi kết luận đó.

Văn phong theo docs/research.md: `[OBSERVED]` đánh dấu điều đã đọc thấy trong file, `[UNRESOLVED]` đánh dấu điều mà việc đọc không xác lập được.
Người viết không chạy gì cả; trong lúc review có chạy một lệnh tra module (xem phần thứ năm).

## OpenSandbox là gì

OpenSandbox là một nền tảng sandbox: một server control plane cùng các agent chạy trong từng sandbox, được điều khiển qua HTTP bằng SDK.
Gói server tự mô tả là "FastAPI control plane for OpenSandbox that manages sandbox lifecycle on Docker (ready) and Kubernetes (planned) runtimes" [OBSERVED] (`opensandbox:server/pyproject.toml:22`).
Nó cần Python 3.10 trở lên [OBSERVED] (`opensandbox:server/pyproject.toml:28`) và kéo theo thư viện client của Docker, FastAPI và Kubernetes trong cây phụ thuộc [OBSERVED] (`opensandbox:server/pyproject.toml:44-50`).
Nhãn độ chín do chính dự án tự gắn là "Development Status :: 3 - Alpha" [OBSERVED] (`opensandbox:server/pyproject.toml:31`).

Các thành phần, theo cấu hình:

| Thành phần | Nguồn | Là gì |
|---|---|---|
| Server lifecycle | `opensandbox:server/pyproject.toml:22`, `opensandbox:server/pyproject.toml:44-50` | Control plane Python/FastAPI, mức Alpha |
| Runtime Docker | `opensandbox:server/configuration.md:105-112` | Đã sẵn sàng; nhận một `execd_image`, có chế độ init tùy chọn |
| Runtime Kubernetes | `opensandbox:server/configuration.md:105-112` | Dự kiến / phương án khác; provider BatchSandbox hoặc agent-sandbox |
| Runtime bảo mật | `opensandbox:server/configuration.md:364-378` | gVisor, Kata, Firecracker tùy chọn, đứng sau các tên OCI runtime / RuntimeClass |
| Egress sidecar + chính sách | `opensandbox:server/configuration.md:231-243`, `opensandbox:specs/sandbox-lifecycle.yml:1917-1926` | Luật cho phép/chặn kết nối ra ngoài cho từng sandbox, chỉ gắn vào khi yêu cầu tạo sandbox có kèm |
| Allowlist thư mục trên máy | `opensandbox:server/configuration.md:291` | Danh sách tiền tố đường dẫn được phép bind mount từ máy; rỗng nghĩa là mọi mount từ máy đều bị từ chối |
| Go SDK | `opensandbox:sdks/sandbox/go/go.mod:1` | Client cho lifecycle, execd và egress, cộng một lớp `Sandbox` bao bên ngoài |

Chọn runtime chỉ bằng một khóa bắt buộc: `runtime.type` là `docker` hoặc `kubernetes` [OBSERVED] (`opensandbox:server/configuration.md:105-112`).
Phần Docker đặt mặc định `network_mode` là `"host"` và ghi `bridge` hoặc một mạng tự định nghĩa là các lựa chọn khác, lưu ý rằng egress sidecar cùng `networkPolicy` cần `bridge` [OBSERVED] (`opensandbox:server/configuration.md:115-130`).
Bản thân sidecar chỉ được gắn khi yêu cầu tạo sandbox có `networkPolicy`, và khi đó phải cấu hình image cho nó [OBSERVED] (`opensandbox:server/configuration.md:231-243`).
Server làm gì với một `networkPolicy` khi đang dùng mạng `host` thì [UNRESOLVED]: tài liệu cấu hình nêu yêu cầu, không nêu cách nó thất bại.
Vậy sandbox mặc định dùng chung network namespace với máy, còn cấu hình mạng có kiểm soát là thứ phải chủ động yêu cầu.

Cô lập mạnh có sẵn dưới dạng tùy chọn, không phải mặc định.
`[secure_runtime]` chọn `gvisor`, `kata` hoặc `firecracker` (Firecracker chỉ dùng được với Kubernetes), ánh xạ sang tên OCI runtime của Docker hoặc RuntimeClass của Kubernetes, với luật kiểm tra hợp lệ cho từng tổ hợp [OBSERVED] (`opensandbox:server/configuration.md:364-378`).
Runtime Docker còn ghi nhận việc bỏ bớt capability, `no-new-privileges` và giới hạn PID như cấu hình server bình thường [OBSERVED] (`opensandbox:server/docker-compose.example.yaml:22-28`).

Có Go SDK, nằm ở `sdks/sandbox/go`.
Một khẳng định đưa ra trong lúc lên kế hoạch cho việc này, rằng không có Go SDK, là sai.
Đường dẫn module của nó là `github.com/alibaba/OpenSandbox/sdks/sandbox/go` [OBSERVED] (`opensandbox:sdks/sandbox/go/go.mod:1`), và trong lúc review, `go list -m -versions` phân giải được cả đường dẫn đó lẫn `github.com/opensandbox-group/OpenSandbox/sdks/sandbox/go` trên proxy.golang.org, từ v1.0.0 đến v1.0.5.
Nó bao cả ba API: một `LifecycleClient` với create/get/list/pause/resume/delete và snapshot [OBSERVED] (`opensandbox:sdks/sandbox/go/lifecycle.go:93`), một `ExecdClient` với session, lệnh chạy nổi/chạy nền, thao tác file và số liệu [OBSERVED] (`opensandbox:sdks/sandbox/go/execd.go:134`) (`opensandbox:sdks/sandbox/go/execd.go:319`) (`opensandbox:sdks/sandbox/go/execd.go:423`), và một `EgressClient` để đọc/sửa/xóa chính sách của sidecar cùng kho credential [OBSERVED] (`opensandbox:sdks/sandbox/go/egress.go:42-54`).
Bên trên là lớp `Sandbox` với các hàm tiện ích cho lệnh, session, file và egress (`opensandbox:sdks/sandbox/go/sandbox.go`) (`opensandbox:sdks/sandbox/go/sandbox_exec.go:23-28`) (`opensandbox:sdks/sandbox/go/sandbox_files.go:83-99`), và các kiểu dữ liệu dùng chung, gồm cả cấu trúc `NetworkPolicy`/`NetworkRule` của egress [OBSERVED] (`opensandbox:sdks/sandbox/go/types.go:132-139`).
Có SDK song song cho Python, JavaScript, C# và Kotlin (`opensandbox:sdks/sandbox/python/pyproject.toml`).
Các ví dụ ở những phần sau dùng SDK Python, nhưng thứ aidev — một chương trình Go — thực sự import sẽ là Go SDK.

Hai hợp đồng API là tài liệu OpenAPI.
Spec lifecycle định nghĩa việc tạo sandbox bằng `image` hoặc `snapshotId`, cộng các trường `volumes`, `networkPolicy`, platform và credential-proxy [OBSERVED] (`opensandbox:specs/sandbox-lifecycle.yml:1792-1799`) (`opensandbox:specs/sandbox-lifecycle.yml:1761-1764`); `networkPolicy` không dùng chung được với cấp phát từ pool [OBSERVED] (`opensandbox:specs/sandbox-lifecycle.yml:1624-1625`).
Mỗi volume gồm một tên, một đường dẫn mount, và đúng một backend — `host`, `pvc`, `ossfs` và các loại khác [OBSERVED] (`opensandbox:specs/sandbox-lifecycle.yml:1792-1799`).
Backend `host` ánh xạ một thư mục trên máy vào container và bị giới hạn bởi allowlist phía server [OBSERVED] (`opensandbox:specs/sandbox-lifecycle.yml:2011-2026`).
Spec execd định nghĩa phong bì sự kiện dạng stream cho việc chạy lệnh và chạy code, với các loại sự kiện `init`, `status`, `error`, `stdout`, `stderr`, `result`, `execution_complete`, `execution_count` và `ping` [OBSERVED] (`opensandbox:specs/execd-api.yaml:1999-2016`).
Đáng chú ý, phong bì đó không có thuộc tính exit code nào; trong spec, exit code chỉ xuất hiện trên đối tượng trạng thái của lệnh chạy nền và của lần chạy code [OBSERVED] (`opensandbox:specs/execd-api.yaml:1977-1986`) (`opensandbox:specs/execd-api.yaml:2410-2414`).
Sự lệch nhau này quan trọng với verification và được phân tích ở phần thứ ba.

## aidev hiện cô lập công việc như thế nào

Ranh giới cô lập của aidev là một git worktree, và chỉ là git worktree.
Đường dẫn worktree được kiểm tra trước khi gọi git: nó phải nằm trong `WORKSPACE_ROOT` và nằm ngoài repository, và chính điều đó biến "agent không chạm được vào thư mục làm việc chính" thành một tính chất của code chứ không phải một quy ước [OBSERVED] (`aidev:internal/git/git.go:160-163`), được thực thi bằng cách phân giải đường dẫn và kiểm tra cô lập trước khi chạy `worktree add` [OBSERVED] (`aidev:internal/git/git.go:175-181`).
Phase 0 đã đo được rằng OpenCode ở chế độ `run` không tương tác ghi file mà không hỏi, nên cờ `--dir <worktree>` là bất biến bắt buộc, không phải tiện ích [OBSERVED] (`aidev:docs/research.md:149-152`).

Mọi thứ khác trong cách chạy agent đều cố ý bình thường.
Backend OpenCode dựng lệnh `run --dir <worktree> --format json` và chạy nó như một tiến trình con [OBSERVED] (`aidev:internal/agent/opencode.go:71`); backend Codex dựng `exec --json --skip-git-repo-check -C <worktree>` theo cùng cách [OBSERVED] (`aidev:internal/agent/codex.go:82`).
Hợp đồng backend yêu cầu agent chạy trong thư mục làm việc của yêu cầu, và báo agent thất bại qua `Result.Status` chứ không phải như một lỗi Go [OBSERVED] (`aidev:internal/agent/agent.go:30-33`) (`aidev:internal/agent/agent.go:150-159`).
Bộ chạy tiến trình cho mỗi tiến trình con một hạn chót, một process group có thể kill cả nhóm, và output bị giới hạn — nhưng nó kế thừa môi trường của người vận hành, vì các công cụ cần `HOME` và `PATH` [OBSERVED] (`aidev:internal/procexec/procexec.go:66-78`), và đặt mỗi tiến trình con vào process group riêng để các tiến trình phụ không bị bỏ lại [OBSERVED] (`aidev:internal/procexec/procexec.go:196-206`).

Nói cụ thể, agent chạy **bằng user của người vận hành, trên máy thật, với mạng và credential của người đó**:

| Khía cạnh cô lập | Điều giữ được | Điều không giữ được |
|---|---|---|
| Hệ thống file | giới hạn trong worktree; thư mục chính không thể chạm tới theo thiết kế (`aidev:internal/git/git.go:160-163`) | không có gì ngoài worktree: agent đọc ghi với UID đó, kể cả các credential nằm quanh `~/.local/share/opencode/auth.json` và bất kỳ file nào UID đó chạm được |
| Tiến trình | process group riêng, SIGTERM rồi mới kill, timeout bắt buộc (`aidev:internal/procexec/procexec.go:196-206`) | chạy không sandbox trên máy; không user namespace, không seccomp, không root chỉ-đọc |
| Mạng | `--sandbox workspace-write` của riêng Codex thêm một lớp bên dưới aidev, chỉ cho backend đó | OpenCode không có chốt chặn mạng; model miễn phí vốn cần mạng, nên quyền truy cập mạng của agent chính là của người vận hành |
| Biến môi trường | bỏ `OTEL_*`; lưu argv (không bao giờ lưu env) để kiểm toán (`aidev:internal/procexec/procexec.go:66-78`) | toàn bộ môi trường được kế thừa, kể cả API key, agent đều nhìn thấy |

Nửa còn lại của thiết kế là người chấm.
Verification chạy các lệnh của chính task và báo exit code của chúng; bản tóm tắt của agent không bao giờ được coi là bằng chứng, và vòng đời task chỉ cho phép đi tới `SUCCEEDED` từ `VERIFYING`, nên kết quả này là thứ duy nhất tạo ra một task thành công [OBSERVED] (`aidev:internal/verification/verification.go:1-8`).
Máy trạng thái cũng khớp: `StatusSucceeded` chỉ đến được từ `StatusVerifying` [OBSERVED] (`aidev:internal/task/status.go:27-30`).
Mỗi bước chạy như một tiến trình con cục bộ trong đúng worktree mà agent đã làm việc [OBSERVED] (`aidev:internal/verification/verification.go:173-186`), và từ chối chạy nếu danh sách bước rỗng [OBSERVED] (`aidev:internal/verification/verification.go:116-128`).

Kiểm tra chặn (interception) là phần liên quan nhất tới chuyện sandbox.
Sau sự cố TASK-000026 — agent viết một file `pytest.py` giả mà verification rồi lại import — aidev liệt kê mọi đường dẫn khác với base commit (kể cả file đã commit và file bị ignore) [OBSERVED] (`aidev:internal/git/changed.go:14-18`) và từ chối mọi bước mà chương trình chấm có thể được nạp từ một đường dẫn đã thay đổi [OBSERVED] (`aidev:internal/verification/interception.go:21-23`).
Worker thực thi việc này giữa lúc agent chạy xong và lúc verification: thu thập đường dẫn thay đổi, tính các chỗ bị chặn, và fail với loại `VERIFICATION` mà không chạy gì nếu danh sách không rỗng [OBSERVED] (`aidev:internal/worker/worker.go:719-731`), sau khi đã chuyển task sang `VERIFYING` [OBSERVED] (`aidev:internal/worker/worker.go:706-714`).
Các rủi ro còn lại đã được ghi nhận là code truyền qua `python -c` / `sh -c`, trình thông dịch ngoài sh/bash/python, và việc import của một script bị che bởi file nằm cạnh nó — kiểm tra này bảo vệ người chấm, không bảo vệ đề thi.

Nói thẳng những gì aidev không bảo vệ: một bước agent độc hại hoặc bị chiếm quyền sẽ chạy với UID, mạng và bí mật của người vận hành, và có thể tuồn dữ liệu ra ngoài, đào coin, hoặc tấn công từ chính máy đó; một phản hồi model độc hại chỉ bị ngăn khỏi thư mục chính, không bị ngăn khỏi việc chạy.
Worktree là bức tường chắn quanh repository, không phải sandbox quanh cái máy.

## OpenSandbox có thể lắp vào đâu

Có hai chỗ một sandbox có thể chen vào — pha agent và pha verification — và hai cách để một worktree đi vào sandbox: volume từ máy, hoặc sao chép file qua execd.
Chúng ảnh hưởng lẫn nhau, và các tổ hợp không ngang giá nhau.

Bind mount từ máy là con đường duy nhất giữ được luồng làm việc dựa trên git của aidev.
API lifecycle có đúng trường này: mỗi volume có backend `host`, đường dẫn mount trong container, và giới hạn allowlist phía server [OBSERVED] (`opensandbox:specs/sandbox-lifecycle.yml:1792-1799`) (`opensandbox:specs/sandbox-lifecycle.yml:2011-2026`).
Nhưng allowlist mặc định rỗng, và khi rỗng thì **mọi** mount từ máy đều bị từ chối theo nguyên tắc an toàn mặc định [OBSERVED] (`opensandbox:server/configuration.md:291`).
Vậy triển khai aidev phải chủ động đưa thư mục gốc của worktree (hoặc từng worktree) vào allowlist, tức là khoét mỗi bản checkout của task vào mount namespace của một container — và ranh giới cô lập khi đó phụ thuộc vào việc allowlist luôn hẹp đúng như dự định, mãi mãi.

Cách còn lại là sao chép: tải worktree lên sandbox bằng API file của execd, rồi tải kết quả về sau.
Go SDK có cả hai chiều (`UploadFile(s)`, `DownloadFile`) cùng với liệt kê thư mục và siêu dữ liệu file [OBSERVED] (`opensandbox:sdks/sandbox/go/sandbox_files.go:83-99`) (`opensandbox:sdks/sandbox/go/execd.go:319`) (`opensandbox:sdks/sandbox/go/execd.go:423`).
Cách này giữ allowlist rỗng, nhưng phá vỡ mọi thứ aidev hiện đang được git cho không:

| Vấn đề | Mount từ máy | Sao chép qua execd |
|---|---|---|
| Diff | `git diff` so với base commit chạy như hiện nay (`aidev:internal/git/git.go:285-293`) | phải dựng lại diff từ các file tải về; xử lý intent-to-add, nhận diện file nhị phân và cắt bớt output đều phải viết lại trên một bản sao |
| Commit khi thành công | commit lên nhánh của task rồi xóa worktree (`aidev:internal/worker/worker.go:862-868`) | commit phải diễn ra hoặc trong sandbox (cần git + danh tính + credential để push ở đó) hoặc bằng cách chép ngược vào worktree rồi commit cục bộ — thêm một bước đồng bộ với những kiểu hỏng riêng của nó |
| Công việc hỏng | worktree được giữ nguyên; việc git tự từ chối xóa là lớp chặn cuối (`aidev:internal/git/git.go:386-399`) | công việc hỏng nằm trong sandbox hoặc snapshot, không nằm trong worktree; giữ lại nghĩa là phải quản lý vòng đời snapshot, một hệ thống sổ sách thứ hai bên cạnh `worktrees` |
| Tính toàn vẹn của người chấm | kiểm tra chặn chạy trên trạng thái git cục bộ (`aidev:internal/worker/worker.go:719-731`) | việc liệt kê đường dẫn thay đổi phải làm trong sandbox hoặc trên cây đã tải về, tức là chính kiểm tra này trở thành một thao tác từ xa |

Không đường nào lắp vào là chạy ngay; đường mount giữ được ngữ nghĩa với cái giá là phải kỷ luật với allowlist, đường sao chép giữ được allowlist với cái giá là phải viết lại cả chuỗi bằng chứng.

Verification trong sandbox gặp một vấn đề gay gắt hơn: quy tắc chỉ có verification của chính aidev mới quyết định thành công.
Hiện nay quy tắc đó được bảo đảm bằng cấu trúc — trạng thái `wait(2)` cục bộ đi vào `procexec.Result.ExitCode` rồi vào bản ghi verification [OBSERVED] (`aidev:internal/verification/verification.go:173-186`).
Qua execd, exit code đến dưới dạng một trường JSON trong sự kiện stream, tức là **dữ liệu không đáng tin đi qua một ranh giới mạng**, được tạo ra bên trong một hệ thống file mà agent vừa ghi vào.
Đưa verification vào sandbox vì thế biến một phép đo cục bộ thành một lời xác nhận từ xa, và người xác nhận lại chính là cỗ máy đang bị chấm.

Cách Go SDK xử lý exit code làm điều này cụ thể hơn.
`Execution.ExitCode` là một con trỏ, bằng nil khi không có [OBSERVED] (`opensandbox:sdks/sandbox/go/execution.go:82-83`).
Một sự kiện `error` gán `Error`, và chỉ gán `ExitCode` khi chuỗi giá trị lỗi phân tích được thành số nguyên [OBSERVED] (`opensandbox:sdks/sandbox/go/execution.go:218-226`).
Một sự kiện `execution_complete` gán `ExitCode` bằng 0 **chỉ khi chưa có exit code nào được ghi và chưa thấy lỗi nào** — comment trong code viết "Foreground command exit code: 0 if no error" [OBSERVED] (`opensandbox:sdks/sandbox/go/execution.go:231-241`).

Điều mà mã nguồn **không** xác lập được là khi nào execd phát sự kiện `error` cho một lệnh thất bại.
Endpoint `/command` ghi rằng nó gửi các sự kiện stdout, stderr, trạng thái chạy và hoàn tất qua SSE [OBSERVED] (`opensandbox:specs/execd-api.yaml:397-406`), nhưng schema `ServerStreamEvent` mà nó trỏ tới không định nghĩa thuộc tính exit code nào — chỉ đối tượng trạng thái của lệnh chạy nền và của lần chạy code mới có [OBSERVED] (`opensandbox:specs/execd-api.yaml:1999-2016`) (`opensandbox:specs/execd-api.yaml:1977-1986`).
Dù vậy, SDK vẫn đọc một trường `exit_code` tùy chọn từ sự kiện stream [OBSERVED] (`opensandbox:sdks/sandbox/go/execution.go:128`) — một trường mà spec không hề hứa sẽ gửi.
Vậy: một lệnh `go test` thất bại trong sandbox có thể hiện ra dưới dạng sự kiện `error` (với `ExitCode` chỉ được gán nếu giá trị tình cờ là số), dưới dạng `execution_complete` kèm một exit code gửi riêng mà spec không ghi, hoặc dưới dạng `execution_complete` không có lỗi và một số 0 do SDK tự suy ra.
[UNRESOLVED] server thực tế làm theo cách nào với một lệnh chạy nổi thoát khác 0 — không chạy gì cả, và spec không nói.
Dựng verification trong sandbox trên nền này nghĩa là thứ quyết định thành công sẽ phụ thuộc vào một ánh xạ không có trong tài liệu.
Cách đọc an toàn là exit code chỉ sống sót qua chuyến đi nhờ quy ước, không nhờ hợp đồng.

Các ví dụ xác nhận hình dạng áp dụng mà dự án hướng tới, và cả giới hạn của nó.
Ví dụ opencode tạo sandbox từ image code-interpreter, cài CLI qua mạng, rồi chạy một prompt trong một thư mục `/tmp` mới bên trong sandbox [OBSERVED] (`opensandbox:examples/opencode/main.py:52-72`); ví dụ codex-cli làm y như vậy, kèm đọc JSONL và tiếp tục session [OBSERVED] (`opensandbox:examples/codex-cli/main.py:79-94`) (`opensandbox:examples/codex-cli/main.py:102-118`); ví dụ claude-code làm tương tự với `-p --output-format json` và `--resume` [OBSERVED] (`opensandbox:examples/claude-code/main.py:77-86`) (`opensandbox:examples/claude-code/main.py:98-111`).
Cả ba mặc định dùng cùng image `code-interpreter:v1.1.0` từ registry Aliyun [OBSERVED] (`opensandbox:examples/opencode/main.py:38-41`) (`opensandbox:examples/codex-cli/main.py:59-62`) (`opensandbox:examples/claude-code/main.py:56-59`).
Không ví dụ nào mount một repository từ máy — mọi lệnh đều chạy trên file đã có sẵn trong image sandbox hoặc được tạo ra ở đó.
Không có ví dụ nào cho đúng thao tác aidev cần: đưa một worktree trên máy vào sandbox, chạy agent trên đó, và lấy về một diff cùng exit code đáng tin.

## Cái giá phải trả

Cái giá được tính bằng niềm tin mới, hạ tầng mới và những kiểu hỏng mới — trong khi brief loại trừ rõ Kubernetes và đề cao một MVP chạy cục bộ.

Server nắm socket của Docker daemon.
File compose mẫu mount `/var/run/docker.sock` vào container server [OBSERVED] (`opensandbox:server/docker-compose.example.yaml:56-57`), cùng với image server và cổng được ánh xạ [OBSERVED] (`opensandbox:server/docker-compose.example.yaml:48-57`).
Socket đó chính là API của Docker: ai ghi được vào nó thì tạo được container, kể cả container privileged với mount tùy ý từ máy — thực chất là quyền root trên máy.
aidev hiện không thêm daemon nào và không có bề mặt privileged nào; áp dụng OpenSandbox sẽ thêm một thứ mà nếu bị chiếm thì cả máy bị chiếm.
File compose còn đặt `resolve_internal = false` kèm comment giải thích rằng server không định tuyến được tới IP bridge của sandbox từ mạng của chính nó [OBSERVED] (`opensandbox:server/docker-compose.example.yaml:8-12`) — cho thấy ngay cả bản triển khai mẫu cũng đang phải lách quanh chuyện mạng của chính nó.

Mặc định về mạng đi ngược với một MVP.
Mặc định toàn server là `network_mode = "host"` [OBSERVED] (`opensandbox:server/configuration.md:119`); bản triển khai mẫu đổi nó thành `bridge`, kèm việc viết lại IP của máy và một dải cấp phát 20.000 cổng [OBSERVED] (`opensandbox:server/docker-compose.example.yaml:22-28`).
Chính sách egress — tính năng có thể giam một agent dùng mạng — cần `bridge` [OBSERVED] (`opensandbox:server/configuration.md:119`).
Vậy mạng có kiểm soát đúng cách là có, nhưng cách mặc định ba quyết định cấu hình, và bản thân image egress là thêm một phiên bản container phải theo dõi [OBSERVED] (`opensandbox:server/docker-compose.example.yaml:22-28`).

Image đến từ những registry aidev không dùng ở đâu khác.
Các ví dụ kéo image code-interpreter từ `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com` [OBSERVED] (`opensandbox:examples/opencode/main.py:38-41`), cấu hình server mẫu ghim image execd vào cùng registry Aliyun đó trong khi image egress và server mặc định dùng tên trên Docker Hub [OBSERVED] (`opensandbox:server/docker-compose.example.yaml:17-20`) (`opensandbox:server/docker-compose.example.yaml:22-28`), và client mẫu là `python:3.11-slim` [OBSERVED] (`opensandbox:server/docker-compose.example.yaml:64-72`).
Áp dụng nghĩa là độ mới, độ trễ và nguồn gốc của bản build giờ phụ thuộc vào một registry ở khu vực Alibaba cộng với Docker Hub, với các phiên bản được ghim (`v1.1.0`, `v1.1.7`, `latest`) rải rác trong các file ví dụ thay vì một lockfile duy nhất.

Xác thực mặc định bị tắt.
`server.api_key` mặc định là null; khi rỗng, server bỏ qua kiểm tra API key và thay vào đó đòi xác nhận rõ ràng về việc chấp nhận chạy không an toàn lúc khởi động (`OPENSANDBOX_INSECURE_SERVER=YES` hoặc xác nhận tương tác) [OBSERVED] (`opensandbox:server/configuration.md:69`).
Một công cụ chạy cục bộ mà người vận hành cũng chính là ranh giới an ninh sẽ phải làm đúng việc này ngay từ đầu — một API lifecycle không xác thực trên `0.0.0.0:8080` — địa chỉ và cổng mặc định [OBSERVED] (`opensandbox:server/configuration.md:67-68`) — là một dịch vụ tạo container mở cho cả mạng LAN.

Và phần lớn dự án là bộ máy mà brief loại trừ.
Kubernetes là một phi mục tiêu rõ ràng của aidev, cùng với scheduler, dashboard, hệ thống xác thực, Kafka và Redis [OBSERVED] (`aidev:docs/architecture.md:330-331`); không có PersistentVolumeClaim vì không có Kubernetes, theo thiết kế [OBSERVED] (`aidev:docs/architecture.md:405-406`), và aidev là công cụ chạy cục bộ cho một người vận hành [OBSERVED] (`aidev:docs/architecture.md:524`).
Runtime Kubernetes của OpenSandbox (provider BatchSandbox hay agent-sandbox, pod template, RuntimeClass, snapshot controller) sẽ là của nợ phải mang theo, trong khi runtime Docker của nó vẫn cần socket, mạng bridge và các registry nói trên.
Cây phụ thuộc của chính server (thư viện client cho Postgres và Redis) và backend lưu trữ `[store]` của nó là thêm những dịch vụ phải vận hành [OBSERVED] (`opensandbox:server/pyproject.toml:55-56`).
[UNRESOLVED] phần lưu trữ đó bắt buộc đến mức nào cho một triển khai tối thiểu, một người vận hành — file compose mẫu chỉ khởi động server, và không chạy gì để biết thiếu phần còn lại thì hỏng gì.

| Chi phí | Quy mô | Tránh được không? |
|---|---|---|
| Daemon giữ docker.sock (tương đương root trên máy) | thêm một dịch vụ privileged | không, gắn liền với runtime Docker |
| Mạng bridge + egress sidecar để giam giữ | 3+ quyết định cấu hình khác mặc định | không, mặc định là mạng host |
| Chuỗi cung ứng image từ registry Aliyun + Docker Hub | phụ thuộc mới về nguồn gốc và độ trễ | một phần, bằng mirror — mà mirror lại là hạ tầng mới |
| API lifecycle mặc định tắt API key | một cái bẫy ngay lần khởi động đầu | có, bằng cách đặt key — nếu người vận hành nhớ làm |
| Provider Kubernetes, CRD, RuntimeClass | cả một hệ thống con không dùng | có, bằng cách phớt lờ — nhưng nó vẫn nằm trên đường phụ thuộc và nâng cấp |
| Server mức Alpha (tự nhận) | rủi ro API thay đổi so với SDK đã ghim | không; ghim phiên bản và kiểm lại mỗi lần nâng |

## Đã chạy gì và chưa chạy gì

Người viết không chạy gì cả.
Trong lúc review có chạy một lệnh: `go list -m -versions` tới proxy.golang.org, để xác nhận đường dẫn module của Go SDK phân giải được.
Không khởi động container nào, không kéo image nào, không cài gói nào, và không dùng mạng — theo ràng buộc của task, và vì bài đánh giá không cần tới chúng.
Mọi khẳng định ở trên đều đến từ việc đọc file: bản clone OpenSandbox tại `d8cfce39dc1d846e580510ca44f44c495cbe95c4` (mỗi đường dẫn được trích đều đã kiểm bằng `git cat-file`) và cây mã aidev trong worktree này.

File đã đọc phía OpenSandbox: `server/pyproject.toml`, `server/configuration.md`, `server/docker-compose.example.yaml`, `specs/sandbox-lifecycle.yml`, `specs/execd-api.yaml`, `sdks/sandbox/go/go.mod`, `sdks/sandbox/go/execution.go`, `sdks/sandbox/go/execd.go`, `sdks/sandbox/go/lifecycle.go`, `sdks/sandbox/go/egress.go`, `sdks/sandbox/go/sandbox.go`, `sdks/sandbox/go/sandbox_exec.go`, `sdks/sandbox/go/sandbox_files.go`, `sdks/sandbox/go/types.go`, `sdks/sandbox/python/pyproject.toml`, `examples/opencode/main.py`, `examples/codex-cli/main.py`, `examples/claude-code/main.py`.
File đã đọc phía aidev: `internal/git/git.go`, `internal/git/changed.go`, `internal/procexec/procexec.go`, `internal/agent/agent.go`, `internal/agent/opencode.go`, `internal/agent/codex.go`, `internal/verification/verification.go`, `internal/verification/interception.go`, `internal/worker/worker.go`, `internal/task/status.go`, `docs/architecture.md`, `docs/research.md`.

Hệ quả, nói rõ để người đọc biết nên tin tới đâu:

- Mọi câu nói về việc server *làm gì khi chạy* (hành vi của socket, định tuyến bridge, thực thi egress, giữ snapshot) đều là diễn giải lại tài liệu cấu hình hoặc hiểu biết chung về Docker, không phải phép đo. Nó được đánh dấu bằng việc trích tài liệu, không phải bằng một lần chạy.
- Phân tích exit code là việc đọc code Go SDK cùng hai spec OpenAPI. Lỗ hổng nó chỉ ra — SDK tự suy ra số 0 ở chỗ spec không hứa gì — nằm ngay trong văn bản, bất kể hôm nay có server nào cư xử đúng hay không.
- [UNRESOLVED] liệu mã thoát khác 0 của một lệnh chạy nổi có luôn đến dưới dạng sự kiện `error`, trường `exit_code`, hay hoàn toàn không đến.
- [UNRESOLVED] liệu `allowed_host_paths` có diễn đạt được "đúng worktree này, vừa tạo năm giây trước" mà không cần khởi động lại server, và liên kết `.git` của một worktree được mount sẽ hoạt động thế nào trong container có UID khác.
- [UNRESOLVED] triển khai server tối thiểu vận hành được (lưu trữ, Redis, image egress) cho một người vận hành trên một máy.
- [UNRESOLVED] liệu các runtime bảo mật (gVisor/Kata) có thay đổi đáng kể điều gì ở trên; chúng mới chỉ được đọc như cấu hình.

## Khuyến nghị

**Hoãn. Chưa áp dụng OpenSandbox cho aidev lúc này — không cho pha agent, không cho verification, không cho một phần nào.**

Bằng chứng ủng hộ kết luận này mà không cần rào đón.
Mối đe dọa mà OpenSandbox nhắm tới — code của agent chạy với UID, mạng và bí mật của người vận hành — là có thật và đã được ghi nhận ở trên.
Nhưng cách chữa được đề xuất lại làm yếu đi chính tính chất mà aidev được xây quanh: thành công được quyết định bởi một phép đo cục bộ (trạng thái `wait` của một tiến trình con trong worktree) mà không output nào của agent tác động được.
Verification trong sandbox thay phép đo đó bằng một trường JSON gửi ra từ bên trong cỗ máy đang bị chấm, qua một giao thức mà chính spec của nó không định nghĩa trường ấy.
Đó không phải người chấm mạnh hơn; đó là người chấm yếu hơn khoác áo cô lập.
Còn sandbox cho agent, dù dễ bảo vệ hơn, lại tốn một daemon privileged, một tư thế mạng khác mặc định, một chuỗi cung ứng image mới, và việc viết lại luồng diff/commit/giữ lại mà git hiện cho không — tất cả chỉ để giam một công cụ một người dùng mà agent vốn đã chạy ở đúng mức quyền của người vận hành.
Worktree vẫn là ranh giới trung thực: nhỏ, đã được kiểm, và được thực thi bằng code.

Những điều phải đúng trước, theo thứ tự:

1. **Một hợp đồng exit code được ghim chặt.** Spec execd (hoặc một phiên bản server được ghim, cộng bộ test do aidev sở hữu) phải nói rõ sự kiện nào mang trạng thái thoát của lệnh chạy nổi, kể cả thoát khác 0, timeout và session bị kill. Chưa có điều đó thì verification trong sandbox không thể thỏa quy tắc chỉ verification của chính aidev mới quyết định thành công.
2. **Một cách triển khai không thêm daemon quyền root mới trên máy của người vận hành.** Hoặc server chạy ở nơi người vận hành không gõ credential của mình (một VM, một máy riêng), hoặc mô hình tin cậy của runtime Docker được chấp nhận rõ ràng và ghi thành tài liệu là nâng yêu cầu về quyền của aidev. Một server Alpha giữ docker.sock trên laptop không phải sandbox; nó là một người vận hành thứ hai, ít được kiểm soát hơn.
3. **Mặc định giam giữ đúng, đã kiểm chứng.** Mạng bridge, egress chặn mặc định nhưng mở cho các endpoint model và gói mà aidev cần, và API key ngay từ lần khởi động đầu — được chứng minh, không chỉ được cấu hình.
4. **Một quyết định về nguồn gốc image.** Tin registry nào, ai mirror, và phiên bản được ghim ở một nơi duy nhất nào.
5. **Một bản thử của luồng mount giữ nguyên ngữ nghĩa git.** Một worktree, nằm trong allowlist, với diff so với base commit, commit khi thành công và giữ lại khi thất bại đều được chứng minh là không đổi — hoặc thừa nhận rõ rằng luồng sao chép phải viết lại chúng.

Nếu một ngày cả năm điều trên đều được thỏa, hình dạng đáng thử vẫn chỉ là sandbox cho pha agent, còn verification giữ ở cục bộ: sandbox giam những gì agent chạm được, còn thứ quyết định vẫn là trạng thái `wait` cục bộ trên các file mà git nhìn thấy.
Verification trong sandbox nên để ngoài bàn kể cả khi đó — nó mâu thuẫn với kiến trúc, không chỉ với tiến độ.
Hãy xem lại khi aidev cần chạy agent không đáng tin (nhiều người dùng, nhiều tenant, hoặc đánh giá model thù địch), thay vì trợ lý của một người vận hành trên một máy.
Cho tới ngày đó, worktree, process group và bộ verification cục bộ là toàn bộ sandbox mà aidev cần.
