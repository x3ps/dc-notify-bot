# DeltaChat Notify Bot

یک پیام‌رسان از webhook به Delta Chat. درخواست‌های HTTP POST را دریافت کرده و آن‌ها را به‌عنوان پیام Delta Chat به گیرندگان تنظیم‌شده ارسال می‌کند. از بارگذاری‌های JSON سازگار با Slack و آپلود فایل از طریق multipart پشتیبانی می‌کند.

## نحوه کار

ربات یک زیرپروسه `deltachat-rpc-server` را (از طریق چارچوب [deltabot-cli-go](https://github.com/deltachat-bot/deltabot-cli-go)) اجرا می‌کند که تمام عملیات IMAP/SMTP مربوط به Delta Chat را مدیریت می‌کند.

در هنگام راه‌اندازی، هر ورودی در `NOTIFY_BOT_RECIPIENTS` به یک چت Delta Chat نگاشت می‌شود:

- **ایمیل ساده** — `CreateContact` + `CreateChatByContactId`. پیام‌ها بلافاصله ارسال می‌شوند اما رمزنگاری سرتاسری ندارند.
- **لینک SecureJoin با پیشوند `OPENPGP4FPR:`** — یک دست‌دهی غیرهمزمان برای تأیید کلید را آغاز می‌کند. چت به‌عنوان «در انتظار» ثبت می‌شود تا زمانی که دست‌دهی کامل شود. ارسال webhook به چت‌های در انتظار با پاسخ `503 Retry-After` نادیده گرفته می‌شود.

سرور HTTP در یک goroutine کنار حلقه رویداد Delta Chat اجرا می‌شود. درخواست‌های دریافتی به تمام گیرندگان آماده ارسال می‌شوند (یا زیرمجموعه خاصی از طریق فیلد `recipient`).

## نصب

### Nix flake

```nix
# flake.nix
{
  inputs.dc-notify-bot.url = "github:x3ps/dc-notify-bot";

  outputs = { nixpkgs, dc-notify-bot, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      modules = [
        dc-notify-bot.nixosModules.default
        {
          services.dc-notify-bot = {
            enable = true;
            email = "bot@example.com";
            passwordFile = "/run/secrets/dc-notify-bot-password";
            recipients = [
              "alice@example.com"
              "OPENPGP4FPR:AABB1234...#a=bob@example.com"
            ];
          };
        }
      ];
    };
  };
}
```

این ماژول یک کاربر سیستمی اختصاصی `dc-notify-bot` ایجاد می‌کند، حساب Delta Chat را در اولین اجرا به‌طور خودکار راه‌اندازی می‌کند و ربات را در یک واحد systemd ایمن اجرا می‌کند.

### Docker

```bash
# دریافت از GitHub Container Registry
docker pull ghcr.io/x3ps/dc-notify-bot:latest

# راه‌اندازی حساب (فقط برای اولین بار)
docker run --rm -v dc-notify-data:/data \
  ghcr.io/x3ps/dc-notify-bot:latest \
  dc-notify-bot -f /data init bot@example.com PASSWORD

# اجرای ربات
docker run -d \
  -v dc-notify-data:/data \
  -e NOTIFY_BOT_RECIPIENTS="alice@example.com" \
  -p 8080:8080 \
  ghcr.io/x3ps/dc-notify-bot:latest
```

### از سورس کد

نیاز به Go نسخه ۱.۲۴ به بالا و `deltachat-rpc-server` در `PATH` دارد.

```bash
git clone https://github.com/x3ps/dc-notify-bot
cd dc-notify-bot
go build -o dc-notify-bot .
```

## استفاده

### بارگذاری JSON (سازگار با Slack)

```bash
# پیام متنی ساده به همه گیرندگان
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello from webhook"}' \
  http://localhost:8080/webhook

# ارسال به یک گیرنده خاص
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello","recipient":"alice@example.com"}' \
  http://localhost:8080/webhook

# ارسال به چند گیرنده خاص
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello","recipients":["alice@example.com","bob@example.com"]}' \
  http://localhost:8080/webhook
```

### آپلود فایل از طریق Multipart

```bash
# متن با فایل پیوست
curl -F 'text=Check this out' -F 'file=@photo.jpg' \
  http://localhost:8080/webhook

# فقط فایل (متن پیش‌فرض "(empty notification)" است)
curl -F 'file=@document.pdf' http://localhost:8080/webhook

# فایل برای گیرنده خاص
curl -F 'text=For you' -F 'file=@photo.jpg' -F 'recipient=alice@example.com' \
  http://localhost:8080/webhook

# ارسال به چند گیرنده
curl -F 'text=Team update' -F 'recipient=alice@example.com' -F 'recipient=bob@example.com' \
  http://localhost:8080/webhook
```

## فیلدهای JSON

| فیلد | اجباری | توضیح |
|------|--------|-------|
| `text` | بله | متن پیام (markdown بدون تغییر ارسال می‌شود) |
| `recipient` | خیر | آدرس ایمیل یک گیرنده خاص (باید در `NOTIFY_BOT_RECIPIENTS` باشد) |
| `recipients` | خیر | آرایه‌ای از آدرس‌های ایمیل گیرندگان (در صورت وجود هر دو، با `recipient` ادغام می‌شود) |

## فیلدهای Multipart

| فیلد | اجباری | توضیح |
|------|--------|-------|
| `text` | یکی از `text` یا `file` اجباری است | متن پیام |
| `file` | یکی از `text` یا `file` اجباری است | فایل پیوست |
| `recipient` | خیر | آدرس ایمیل یک گیرنده خاص (برای چند گیرنده قابل تکرار است) |

## پاسخ‌های خطا

| وضعیت | معنا |
|-------|------|
| 400 | JSON نامعتبر، فیلدهای اجباری وجود ندارند، یا گیرنده ناشناخته |
| 405 | متد مجاز نیست |
| 413 | حجم بارگذاری بیش از حد مجاز است |
| 415 | Content-Type پشتیبانی نمی‌شود (از `application/json` یا `multipart/form-data` استفاده کنید) |
| 500 | تمام تلاش‌های ارسال پیام ناموفق بودند |
| 503 | تمام گیرندگان در انتظار تکمیل دست‌دهی SecureJoin هستند (شامل هدر `Retry-After`) |

## نقاط پایانی (Endpoints)

- `POST /webhook` — ارسال اعلان
- `GET /health` — بررسی سلامت سرویس
- `GET /recipients` — فهرست گیرندگان تنظیم‌شده با وضعیت آن‌ها

## پیکربندی

| متغیر محیطی | پیش‌فرض | توضیح |
|-------------|---------|-------|
| `NOTIFY_BOT_RECIPIENTS` | (اجباری) | فهرست گیرندگان جدا شده با کاما: آدرس‌های ایمیل یا URI های SecureJoin با پیشوند `OPENPGP4FPR:` |
| `NOTIFY_BOT_LISTEN` | `127.0.0.1:8080` | آدرس شنود HTTP |
| `NOTIFY_BOT_MAX_PAYLOAD_BYTES` | `1048576` (1 MiB) | حداکثر اندازه بدنه درخواست |

نیاز به `deltachat-rpc-server` در `PATH` دارد.

## SecureJoin

SecureJoin یک چت رمزنگاری‌شده سرتاسری با تأیید کلید برقرار می‌کند. برای دریافت لینک دعوت `OPENPGP4FPR:` از برنامه Delta Chat:

1. Delta Chat را باز کنید ← **تنظیمات** ← **دعوت به Delta Chat** (یا **کد QR**)
2. روی **کپی لینک** ضربه بزنید تا URI `OPENPGP4FPR:…` را دریافت کنید
3. URI را به `NOTIFY_BOT_RECIPIENTS` اضافه کنید

در اولین تماس، ربات یک دست‌دهی SecureJoin آغاز می‌کند. چت به‌عنوان «در انتظار» علامت‌گذاری می‌شود تا زمانی که گیرنده در کلاینت Delta Chat خود درخواست را بپذیرد و پروتکل به‌طور خودکار تکمیل شود. پس از آن، مخاطب تأیید شده و پیام‌ها رمزنگاری سرتاسری دارند.

در راه‌اندازی‌های بعدی، یک مخاطب تأیید‌شده بلافاصله شناخته می‌شود (نیازی به دست‌دهی جدید نیست) و چت آماده استفاده است.

## توسعه

```bash
# ورود به محیط توسعه با Go toolchain، gopls و deltachat-rpc-server
nix develop

# اجرای تست‌ها
go test -v ./...

# ساخت
nix build

# تست دستی
dc-notify-bot init bot@example.com PASSWORD
dc-notify-bot serve &
curl -X POST http://127.0.0.1:8080/webhook \
  -H 'Content-Type: application/json' \
  -d '{"text":"hello"}'
```

## مجوز

[Mozilla Public License 2.0](https://www.mozilla.org/en-US/MPL/2.0/)
