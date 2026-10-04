# GhostLink Go client

Кроссплатформенный (Linux) консольный клиент для обмена сообщениями с приложением
**GhostLink** по Bluetooth (SPP, RFCOMM). Это «младший брат» Android-приложения
GhostLink — та же логика, та же протокольная спецификация, только на Go, работающая
непосредственно со стеком BlueZ.

[Android-приложение GhostLink](https://github.com/CapMarkoTo/Ghostlink_) ·
[Как это работает](#как-это-работает) ·
[Спецификация протокола](#спецификация-протокола) ·
[Установка и запуск](#установка-и-запуск) ·
[Синтаксис команд](#синтаксис-команд)

## Описание

GhostLink — это приложение для локального общения по Bluetooth.
Android-приложение (см. [CapMarkoTo/Ghostlink_](https://github.com/CapMarkoTo/Ghostlink_))
разворачивает RFCOMM-сервер SPP-канала, а этот Go-клиент соединяется с ним
с Linux-машины через D-Bus интерфейс BlueZ, получая обычный потоковый `socket`.

Клиент не требует Root и не паркует сокет — он использует штатные утилиты BlueZ через
системную шину D-Bus:

1. `org.bluez.Adapter1.StartDiscovery()` — сканируем сеть и находим телефон,
   рекламирующий UUID сервиса SPP.
2. `org.bluez.Device1.Connect()` — BlueZ сам устанавливает RFCOMM-соединение.
3. `org.bluez.SerialPort1` появляется под путём устройства и сообщает номер
   RFCOMM-канала → путь к `/dev/rfcomm<N>`, обычному потоковому файлу.

## Как это работает

### Роль каждого участника

| Компонент | Роль | Стек |
|---|---|---|
| Android (GhostLink, `MainActivity`) | **сервер**: `listenUsingRfcommWithServiceRecord("GhostLinkBT", MY_UUID)` | Android Bluetooth API |
| Android (`MessageActivity`) | читает/пишет сообщение в поток сокета | — |
| **Go client (`main.go`)** | **клиент**: соединяется через D-Bus / BlueZ, читает `stdin` → пишет в `/dev/rfcomm<N>` | BlueZ + D-Bus + raw socket |

### Шаг за шагом

1. **Сканирование.** Программа включит адаптер (если выключен), запустит
   `StartDiscovery()` и подо́бёт список `org.bluez.Device1`, фильтруя по UUID
   сервиса:
   ```
   SPP_UUID = 8ce255c0-200a-11e0-ac64-0800200c9a66   // MY_UUID в Android
   ```
   Найдя устройство, оно выводит его имя и MAC и выбирает его как цель.

2. **Подключение.** Вызывается `Device1.Connect()`; далее Go-программа опрашивает
   `GetManagedObjects()`, пока под путём устройства не появится интерфейс
   `org.bluez.SerialPort1` с свойством `Path` (номер канала). Затем открывается
   `/dev/rfcomm<N>` — и соединение установлено.

3. **Чат.** Работает точная копия того, что делает `MessageActivity`:
   - **отправка**: строка из `stdin` → `os.Write` UTF-8-байтами, **без переноса** и
     без любого обрамления (в `MessageActivity` то же:
     `outputStream.write(message.toByteArray(Charsets.UTF_8))`).
   - **получение**: каждый пришедший чанк `Read()` — это одно полное сообщение
     (`MessageActivity` читает в буфер 1024 байта и превращает в `String` целиком).

Соединение завершается по `Ctrl-C` (закрывает сокет и `stdin`), либо по `Ctrl-D` /
EOF со стороны stdin (с задержкой 500 мс, чтобы отобразить «дошедшее» сообщение).

## Спецификация протокола

Протокол — это **голые UTF-8-байты**. Никаких заголовков, никаких префиксов длины,
никаких разделителей.

| Направление | Формат | Замечание |
|---|---|---|
| Android → клиент | `write(message.toByteArray(Charsets.UTF_8))` | `MessageActivity::sendMessage` |
| клиент → Android | `f.Write([]byte(msg))` | ровно столько байт, сколько в строке; `\n` не добавляется |
| Прием (обе стороны) | каждый `read()` → новое сообщение | размер чанка не важен (1024 / 4096 / 1400 — любое разбивание) |

Следовательно: **не нужно** читать до `\n` или пока не закончится чанк — каждое
сообщение приходит отдельным вызовом чтения, ровно как и отправляется.

### UUID и имена

- Имя сервиса Bluetooth: `GhostLinkBT`
- UUID SPP-сервиса: `8ce255c0-200a-11e0-ac64-0800200c9a66` (должен совпадать с
  `MY_UUID` в `MainActivity.kt` и в `createRfcommSocketToServiceRecord`
  `DeviceListActivity.kt`)

## Установка и запуск

### Требования

- Linux + BlueZ (`bluetoothd`)
- включённый Bluetooth-адаптер
- пользователь в группе `dialout` (иначе «open /dev/rfcommN: permission denied»)
- Go 1.27 (указано в `go.mod`; пакет `github.com/godbus/dbus/v5 v5.2.2`)

```bash
cd ghostlink-go-client
go build -o ghostlink .
```

### Режимы работы

```bash
./ghostlink                       # сканировать ~15 с, найти GhostLink-устройство,
                                  # подключиться и начать чат
./ghostlink -list                 # только перечислить устройства, рекламирующие
                                  # SPP-сервис GhostLink (без сканирования)
./ghostlink -addr AA:BB:CC:DD:EE:FF   # пропустить сканирование и подключиться
                                  # напрямую по MAC (устройство должно быть
                                  # спарено, иначе BlueZ отвергнет)
./ghostlink -scan 30s             # изменить время сканирования
```

Типичный сценарий:

1. На телефоне: запустить **GhostLink**, нажать **«Host»** (кнопка `btnHost`).
   Телефон станет видимым на 300 с и начнёт ожидать подключение.
2. На Linux: `./ghostlink` — он найдёт телефон и откроет чат.
3. Пишите сообщения в терминал — они появятся на экране телефона, и наоборот.

### Среда для тестирования без железа

Можно указать произвольную директорию, где лежат файлы `/rfcomm<N>` (например,
FIFO или сокеты-заглушки):

```bash
mkdir /tmp/rfcomm-test
GHOSTLINK_RFCOMM_DIR=/tmp/rfcomm-test ./ghostlink
```

## Интерфейс чата

Сообщения раскрашиваются:

```
type a message and press Enter (Ctrl-D to exit)
```

```
phone: <текст с телефона>
me: <ваше сообщение>
```

- Входящие — синий цвет, с новой строки (в `MessageActivity` они тоже выводятся
  в отдельном чате-экране).
- Выходящие — зелёный, сразу за вводом.

## Структура проекта

```
ghostlink-go-client/
├── main.go            # весь клиент: BlueZ-прокси, диспетчер соединений, чат
├── go.mod             # модуль ghostlink/client, deps: godbus/dbus/v5
├── go.sum
├── README.md
└── LICENSE            # Apache-2.0 (как и в Ghostlink_)
```

Единственный файл `main.go` разделён комментариями на логические блоки: BlueZ
D-Bus-прокси, обнаружение устройств, подключение, цикл чата, `main` с флагами.

## Зависимости

| Пакет | Назначение |
|---|---|
| `github.com/godbus/dbus/v5` | системная шина D-Bus, интерфейс BlueZ (`org.bluez`) |
| `golang.org/x/sys` | низкоуровневые вызовы ядра (поддерживающий) |

## Отличия от Android-версии

| | Android (`MessageActivity`) | Go client (`main.go`) |
|---|---|---|
| Подключение | `createRfcommSocketToServiceRecord()` + `connect()` | D-Bus `Device1.Connect()` + `SerialPort1.Path` → `/dev/rfcomm<N>` |
| Буфер чтения | `byte[1024]` | `byte[4096]` |
| Отображение | `ListView` + `MessageAdapter` | раскрашенный `printf` в терминал |
| Закрытие | `isListening` + `inputStream.close()` | `ctx.Done()` закрывает сокет + `Ctrl-D` |
| Обработка ввода | `EditText` | `bufio.Scanner(os.Stdin)` (одна строка = одно сообщение) |

Логика **одной строки = одного сообщения без обрамления** идентична — это основа
взаимодействия.

## Лицензия

[Apache License 2.0](LICENSE) — как и в родительском проекте `Ghostlink_`.
