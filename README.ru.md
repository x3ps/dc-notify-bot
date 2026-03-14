# DeltaChat Notify Bot

Перенаправитель webhook-сообщений в Delta Chat. Принимает HTTP POST-запросы и доставляет их как сообщения Delta Chat настроенным получателям. Поддерживает JSON-нагрузки в формате Slack и загрузку файлов через multipart.

## Как это работает

Бот запускает дочерний процесс `deltachat-rpc-server` (через фреймворк [deltabot-cli-go](https://github.com/deltachat-bot/deltabot-cli-go)), который обрабатывает все операции Delta Chat по IMAP/SMTP.

При запуске каждая запись в `NOTIFY_BOT_RECIPIENTS` сопоставляется с чатом Delta Chat:

- **Обычный email** — `CreateContact` + `CreateChatByContactId`. Сообщения отправляются немедленно, но без сквозного шифрования.
- **Ссылка SecureJoin `OPENPGP4FPR:`** — запускает асинхронное рукопожатие с верификацией ключа. Чат помечается как «ожидающий» до завершения рукопожатия. Доставка webhook-сообщений в ожидающие чаты пропускается с ответом `503 Retry-After`.

HTTP-сервер работает в горутине параллельно с циклом событий Delta Chat. Входящие запросы рассылаются всем готовым получателям (или конкретному подмножеству через поле `recipient`).

## Установка

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

Модуль создаёт выделенного системного пользователя `dc-notify-bot`, автоматически инициализирует аккаунт Delta Chat при первом запуске и запускает бота в защищённом юните systemd.

### Docker

```bash
# Загрузить из GitHub Container Registry
docker pull ghcr.io/x3ps/dc-notify-bot:latest

# Инициализировать аккаунт (только при первом запуске)
docker run --rm -v dc-notify-data:/data \
  ghcr.io/x3ps/dc-notify-bot:latest \
  dc-notify-bot -f /data init bot@example.com PASSWORD

# Запустить бота
docker run -d \
  -v dc-notify-data:/data \
  -e NOTIFY_BOT_RECIPIENTS="alice@example.com" \
  -p 8080:8080 \
  ghcr.io/x3ps/dc-notify-bot:latest
```

### Из исходного кода

Требуется Go 1.24+ и `deltachat-rpc-server` в `PATH`.

```bash
git clone https://github.com/x3ps/dc-notify-bot
cd dc-notify-bot
go build -o dc-notify-bot .
```

## Использование

### JSON-нагрузка (совместимая со Slack)

```bash
# Простое текстовое сообщение всем получателям
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello from webhook"}' \
  http://localhost:8080/webhook

# Отправить конкретному получателю
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello","recipient":"alice@example.com"}' \
  http://localhost:8080/webhook

# Отправить нескольким конкретным получателям
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello","recipients":["alice@example.com","bob@example.com"]}' \
  http://localhost:8080/webhook
```

### Загрузка файлов через multipart

```bash
# Текст с прикреплённым файлом
curl -F 'text=Check this out' -F 'file=@photo.jpg' \
  http://localhost:8080/webhook

# Только файл (текст по умолчанию — "(empty notification)")
curl -F 'file=@document.pdf' http://localhost:8080/webhook

# Файл конкретному получателю
curl -F 'text=For you' -F 'file=@photo.jpg' -F 'recipient=alice@example.com' \
  http://localhost:8080/webhook

# Отправить нескольким получателям
curl -F 'text=Team update' -F 'recipient=alice@example.com' -F 'recipient=bob@example.com' \
  http://localhost:8080/webhook
```

## Поля JSON

| Поле | Обязательное | Описание |
|------|--------------|----------|
| `text` | Да | Текст сообщения (markdown передаётся как есть) |
| `recipient` | Нет | Email конкретного получателя (должен быть в `NOTIFY_BOT_RECIPIENTS`) |
| `recipients` | Нет | Массив email-адресов получателей (объединяется с `recipient`, если оба указаны) |

## Поля multipart

| Поле | Обязательное | Описание |
|------|--------------|----------|
| `text` | Одно из `text` или `file` обязательно | Текст сообщения |
| `file` | Одно из `text` или `file` обязательно | Прикреплённый файл |
| `recipient` | Нет | Email конкретного получателя (можно повторить для нескольких) |

## Коды ошибок

| Статус | Значение |
|--------|----------|
| 400 | Некорректный JSON, отсутствуют обязательные поля или неизвестный получатель |
| 405 | Метод не разрешён |
| 413 | Нагрузка слишком большая |
| 415 | Неподдерживаемый Content-Type (используйте `application/json` или `multipart/form-data`) |
| 500 | Все попытки отправить сообщение завершились неудачей |
| 503 | У всех получателей ожидается завершение рукопожатия SecureJoin (включает заголовок `Retry-After`) |

## Эндпоинты

- `POST /webhook` — Отправить уведомление
- `GET /health` — Проверка работоспособности
- `GET /recipients` — Список настроенных получателей со статусом

## Конфигурация

| Переменная окружения | По умолчанию | Описание |
|----------------------|--------------|----------|
| `NOTIFY_BOT_RECIPIENTS` | (обязательно) | Список получателей через запятую: email-адреса или SecureJoin URI `OPENPGP4FPR:` |
| `NOTIFY_BOT_LISTEN` | `127.0.0.1:8080` | Адрес HTTP-сервера |
| `NOTIFY_BOT_MAX_PAYLOAD_BYTES` | `1048576` (1 МиБ) | Максимальный размер тела запроса |

Требуется `deltachat-rpc-server` в `PATH`.

## SecureJoin

SecureJoin устанавливает сквозное зашифрованное общение с верификацией ключа. Чтобы получить ссылку-приглашение `OPENPGP4FPR:` из приложения Delta Chat:

1. Откройте Delta Chat → **Настройки** → **Пригласить в Delta Chat** (или **QR-код**)
2. Нажмите **Копировать ссылку**, чтобы получить URI `OPENPGP4FPR:…`
3. Добавьте URI в `NOTIFY_BOT_RECIPIENTS`

При первом контакте бот инициирует рукопожатие SecureJoin. Чат помечается как «ожидающий» до тех пор, пока получатель не примет запрос в своём клиенте Delta Chat и протокол не завершится автоматически. После этого контакт верифицирован и сообщения шифруются сквозным шифрованием.

При последующих перезапусках верифицированный контакт распознаётся немедленно (новое рукопожатие не требуется) и чат готов к работе.

## Разработка

```bash
# Войти в среду разработки с Go toolchain, gopls и deltachat-rpc-server
nix develop

# Запустить тесты
go test -v ./...

# Собрать
nix build

# Ручное тестирование
dc-notify-bot init bot@example.com PASSWORD
dc-notify-bot serve &
curl -X POST http://127.0.0.1:8080/webhook \
  -H 'Content-Type: application/json' \
  -d '{"text":"hello"}'
```

## Лицензия

[Mozilla Public License 2.0](https://www.mozilla.org/en-US/MPL/2.0/)
