# Implementasi Provider Claude Subscription dan Routing Berbasis Model

## Ringkasan

Tambahkan provider OAuth `anthropic-claude` untuk Claude Pro/Max, dengan beberapa koneksi, refresh token, quota, failover otomatis, CLI, dan pengelolaan penuh melalui TUI. Jika akun utama gagal dengan error retryable, router memberi cooldown, mencoba akun berikutnya, lalu mempromosikan akun pengganti setelah berhasil agar request selanjutnya langsung menggunakannya.

Anthropic mendokumentasikan bahwa Claude Code dapat menggunakan login subscription melalui `ANTHROPIC_BASE_URL` selama gateway meneruskan body, capability OAuth, dan header protokol yang diperlukan. Router harus bertindak sebagai proxy transparan tanpa spoof header, tool cloaking, atau system-prompt overlay. Auto-fallback antar-subscription tetap dapat dipandang sebagai penggabungan kapasitas akun, sehingga fitur wajib menampilkan konfirmasi risiko dan tidak boleh mengklaim dapat mencegah pembatasan akun. Lihat [gateway Anthropic](https://code.claude.com/docs/en/llm-gateway), [protocol reference](https://code.claude.com/docs/en/llm-gateway-protocol), [Consumer Terms](https://www.anthropic.com/legal/consumer-terms), dan [implementasi Claude 9router](https://github.com/decolua/9router/blob/master/open-sse/providers/registry/claude.js).

## Perubahan Utama

### Provider, OAuth, dan penyimpanan

- Gunakan ID provider:
  - `openai-codex` untuk akun yang sudah ada.
  - `anthropic-claude` untuk Claude subscription.
  - CLI menerima alias input `claude`, tetapi menyimpan ID kanonis.
- Implementasikan OAuth Claude Code:
  - Authorization: `https://claude.ai/oauth/authorize`.
  - Token: `https://api.anthropic.com/v1/oauth/token`.
  - Public client ID: `9d1c250a-e61b-44d9-88ed-5944d1962f5e`.
  - Scope: `org:create_api_key user:profile user:inference`.
  - PKCE S256, state CSRF, dan token exchange/refresh berbentuk JSON.
  - Input penyelesaian login menerima callback URL maupun format `code#state`.
  - Simpan access token, rotated refresh token, expiry, scope, dan status re-auth pada tabel akun.
- Tambahkan state retry persisten per akun: `consecutive_failures`, `last_failure_at`, dan `cooldown_until`. Migrasi SQLite harus idempotent dan mempertahankan seluruh data database lama.
- Generalisasikan token manager agar mendukung refresher per provider, locking per akun, refresh lead berbeda, dan permanent refresh error.
  - Codex mempertahankan perilaku sekarang.
  - Claude refresh proaktif empat jam sebelum expiry, mengikuti 9router.
  - `invalid_grant` dan token permanen tidak valid menandai koneksi `NeedsReauth`.
- Alias unik hanya dalam provider yang sama. Tambahkan lookup dan validasi `(provider, name)`.
- Priority dihitung dan diurutkan per provider; akun Claude dan Codex tidak saling memengaruhi.

### Routing dan upstream Claude

- Routing model bersifat case-insensitive setelah trim:
  - Prefix `gpt` menuju pool `openai-codex`.
  - Prefix `claude` menuju pool `anthropic-claude`.
  - Prefix lain menghasilkan HTTP 400 dengan pesan model/provider tidak didukung.
- Matriks endpoint:
  - `/v1/messages` + `claude*`: kirim body native tanpa translasi ke `https://api.anthropic.com/v1/messages?beta=true`.
  - `/v1/messages` + `gpt*`: pertahankan translasi Anthropic ke Responses ke Codex yang sudah ada.
  - `/v1/messages/count_tokens` + `claude*`: proxy native ke Anthropic.
  - `/v1/messages/count_tokens` + `gpt*`: gunakan estimator lokal yang sudah ada.
  - `/v1/responses` dan `/v1/chat/completions`: hanya menerima `gpt*`; `claude*` mendapat 400 yang mengarahkan penggunaan ke `/v1/messages`.
- Untuk request Claude:
  - Pertahankan body Claude Code apa adanya, termasuk system prompt, thinking, tool calls, cache controls, dan metadata.
  - Teruskan keluarga header `anthropic-*` sebagai open list tanpa allowlist nilai beta; teruskan juga identitas client asli seperti `user-agent`, `x-app`, `x-stainless-*`, dan `x-claude-code-*` tanpa membuat nilai sintetis.
  - Ganti auth lokal dengan `Authorization: Bearer <provider OAuth token>` dan jangan meneruskan local API key.
  - Pastikan `anthropic-version: 2023-06-01`; pertahankan seluruh beta client dan tambahkan hanya capability OAuth/Claude Code yang diperlukan karena router mengganti credential.
  - Jangan menerapkan tool cloaking, device metadata palsu, atau fingerprint statis 9router. Karena targetnya adalah Claude Code asli, teruskan body dan identitas client yang benar tanpa mengganti nama tool, membuat identitas perangkat sintetis, atau menyamar sebagai versi client tertentu.
- Respons non-streaming dan SSE Claude diteruskan native, termasuk status, content type, request ID, rate-limit, dan retry headers.
- Failover memakai priority dalam pool provider:
  - Retry akun berikutnya untuk network error, `429`, `5xx`, serta `401/403` setelah satu refresh-and-retry.
  - Jangan failover untuk request error `4xx` lainnya.
  - `Retry-After` atau rate-limit reset header menentukan cooldown jika tersedia. Tanpa header, gunakan exponential backoff mulai dua detik, maksimum lima menit, dengan jitter.
  - Increment `consecutive_failures` setiap kegagalan retryable; keberhasilan mereset failure state akun tersebut.
  - Persistent `401/403` setelah refresh menandai akun `NeedsReauth` sebelum mencoba akun berikutnya.
  - Simpan akun pertama yang benar-benar dicoba. Jika fallback berikutnya mendapat respons `2xx`, tukar priority akun sukses dengan akun pertama yang gagal secara atomik. Dengan lebih dari dua akun, akun di antaranya mempertahankan priority.
  - Priority swap memakai compare-and-swap terhadap nilai priority yang dibaca saat routing agar request paralel tidak melakukan double-swap atau membalik urutan kembali.
  - Jika semua akun gagal, jangan ubah priority; pertahankan cooldown masing-masing dan teruskan status, header, serta body error upstream terakhir.
  - `/v1/messages/count_tokens` boleh fallback tetapi tidak mengubah priority agar traffic startup/token-count tidak mengacak urutan akun.
  - Setelah stream 2xx mulai dikirim ke client, jangan berpindah akun di tengah stream.
- Tambahkan model Claude yang saat ini diiklankan 9router ke `/v1/models`, tetapi jangan gunakan katalog tersebut sebagai validasi; semua nama berprefix `claude` tetap pass-through.

### TUI, CLI, dan konfigurasi Claude Code

- Ubah alur TUI menjadi:
  - `Providers`
    - `OpenAI Codex`
    - `Anthropic Claude`
  - Setiap provider membuka daftar koneksinya sendiri dengan `Add New Connection`.
- Kedua jenis provider memiliki menu koneksi yang sama:
  - Edit Alias
  - Test Connection
  - Show Quota Limit
  - Refresh Token
  - Re-authenticate
  - Enable/Disable
  - Delete Connection
  - Reorder dengan Shift+Up/Down
- Sebelum OAuth Claude, tampilkan halaman risiko dengan pilihan `Cancel` dan `I understand, continue`.
- Simpan URL OAuth Claude ke `~/.lm-router/anthropic-claude-auth-url.txt`.
- `Test Connection` Claude memvalidasi token melalui endpoint OAuth usage tanpa mengirim prompt inference. Respons quota `429` berarti token masih dianggap terhubung tetapi quota sementara tidak tersedia.
- Quota Claude membaca `https://api.anthropic.com/api/oauth/usage`, menampilkan window 5 jam, mingguan, dan model-specific weekly windows. Cooldown quota selama tiga menit tidak memblokir inference.
- Tambahkan CLI:
  - `lm-router auth add anthropic-claude --name main`.
  - `--accept-risk` untuk non-interactive confirmation.
  - `auth list --provider <provider>`.
  - `auth test/refresh` dispatch berdasarkan provider akun.
  - Lookup `--name` membutuhkan `--provider` agar tidak ambigu.
- Tambahkan `lm-router claude print-config` dan layar `Claude Config` yang mencetak, tanpa otomatis mengubah file pengguna:
  - `ANTHROPIC_BASE_URL=http://127.0.0.1:19090`.
  - `ANTHROPIC_AUTH_TOKEN=<local lm-router key>`.
  - Optional `ANTHROPIC_MODEL=<claude-prefixed model>`.
- Perbarui README dengan setup OAuth Claude, konfigurasi Claude Code, aturan prefix routing, cooldown/backoff, auto priority swap, quota, dan peringatan bahwa auto-rotation menggabungkan kapasitas beberapa subscription serta tidak menjamin akun bebas dari tindakan provider.

## Pengujian

- OAuth: parameter PKCE/state/scope, callback URL dan `code#state`, exchange JSON, refresh-token rotation, expiry, serta permanent refresh failure.
- Store/service: filtering provider, alias yang sama lintas provider, penolakan duplikat dalam provider, priority independen, migrasi retry state, cooldown persisten, reset failure state, atomic priority swap, dan dispatch test/refresh.
- Claude client: body tidak berubah, open-list header forwarding, local key tidak bocor, beta merging, redaksi log, non-streaming, SSE, refresh-on-401, prioritas reset header, exponential backoff hingga batas lima menit, jitter, dan retry classification.
- Proxy route matrix untuk setiap endpoint dengan model `gpt*`, `claude*`, unknown prefix, pool kosong, serta kegagalan setelah stream dimulai.
- Auto-rotation dua akun: A gagal retryable, B sukses, priority tertukar, dan request berikutnya langsung memakai B tanpa mencoba A yang cooldown.
- Auto-rotation banyak akun: akun sukses ditukar dengan akun pertama yang gagal, sedangkan akun di antaranya tidak berubah.
- Network error, `429`, `5xx`, dan persistent `401/403` memicu fallback; `4xx` lain berhenti tanpa cooldown atau swap.
- Jika semua akun gagal, priority tidak berubah dan error upstream terakhir diteruskan. `count_tokens` dapat fallback tanpa mengubah priority.
- Dua request paralel tidak dapat melakukan double-swap. Respons streaming yang telah dimulai tidak pernah diteruskan ulang ke akun lain.
- TUI: pemilihan jenis provider, warning confirmation, daftar terfilter, add/reauth, semua connection actions, quota Claude, dan reorder dalam provider.
- CLI/config: provider aliases, `--accept-risk`, lookup provider+name, dan output environment Claude Code.
- Jalankan seluruh `go test ./...`; baseline saat perencanaan adalah 105 test lulus.
- Manual acceptance:
  - Hubungkan minimal satu akun Claude subscription.
  - Jalankan Claude Code dengan base URL dan local key router.
  - Model `claude*` terdeteksi di log menuju akun Claude.
  - Model `gpt*` dari `/v1/messages` menuju akun Codex.
  - Tool use, thinking, prompt caching, streaming, token counting, refresh, quota, dan failover bekerja end-to-end.

## Asumsi dan Batasan

- V1 hanya mendukung Claude melalui native Anthropic Messages API; tidak ada translasi Claude ke Responses atau Chat Completions.
- Alias seperti `sonnet`, `opus`, `fable`, atau `haiku` diasumsikan diselesaikan Claude Code menjadi nama penuh `claude-*`; router sendiri hanya menentukan provider dari prefix yang dikirim.
- Fitur tidak mengimpor credential Claude Code yang sudah tersimpan dan tidak memalsukan identitas perangkat.
- Tidak ada automated live-OAuth test di CI; seluruh network test menggunakan mock server.
- Auto priority swap hanya berlaku untuk pool `anthropic-claude` dan baru disimpan setelah akun pengganti mendapat respons `2xx`.
- Semua error retryable mencakup network error walaupun terdapat risiko request sudah diterima upstream sebelum koneksi terputus; implementasi dan dokumentasi harus menyebut kemungkinan duplikasi ini.
- Pengguna wajib menerima peringatan bahwa auto-fallback antar-subscription dapat dianggap menggabungkan atau melewati batas kapasitas provider dan berisiko membatasi akun.
- Implementasi tetap berada di branch `feat/anthropic-claude-provider` dan tidak boleh di-commit sebelum review pengguna.
