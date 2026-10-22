# Диагностический журнал (с v1.19.33-urltestfix4)

Встроенная замена `mihomo-monitor` плюс подробности для разбора работы url-test, fallback,
health-check и провайдера.

## Как устроен

* **Выключен по умолчанию.** Включается переменной окружения `MIHOMO_DIAG_DIR` в службе systemd; конфиг
  mihomo не меняется, `-t` и панель ничего не замечают. Без переменной сборка ведёт себя как urltestfix3.
* **Не зависит от `log-level`.** Можно держать `info`/`warning`, как удобно.
* **Свои файлы** `diag-ГГГГ-ММ-ДД.log`, по одному на день (по местному времени контейнера). Прошедшие дни
  сжимаются в `.log.gz`, файлы старше 14 дней удаляются. Объём — порядка 10–15 МБ в день до сжатия
  (~1,5 МБ после) при ~146 узлах.
* **Без данных гостей.** В журнал не пишутся адреса клиентов и назначения соединений (никаких сайтов),
  ссылка подписки и secret. В текстах ошибок URL, IP-адреса и `host:port` заменяются на `<url>`, `<ip>`,
  `<host>`. Имена узлов пишутся как есть.
* Предупреждения (Δ больше tolerance) дублируются в обычный журнал mihomo строкой `[Diag] …` уровня warning
  — как «ВНИМАНИЕ» у mihomo-monitor.

Переменные:

| переменная | по умолчанию | что |
|---|---|---|
| `MIHOMO_DIAG_DIR` | — (выключено) | каталог файлов, включает журнал |
| `MIHOMO_DIAG_INTERVAL` | `1m` | период строк состояния (`30s`, `2m`, …) |
| `MIHOMO_DIAG_KEEP_DAYS` | `14` | сколько дней хранить |

## Включить (в контейнере 101)

```
mkdir -p /etc/systemd/system/mihomo.service.d
cat > /etc/systemd/system/mihomo.service.d/diag.conf <<'EOF'
[Service]
Environment=MIHOMO_DIAG_DIR=/var/log/mihomo-diag
EOF
systemctl daemon-reload && systemctl restart mihomo
# через минуту:
tail -n 20 /var/log/mihomo-diag/diag-$(date +%F).log
journalctl -u mihomo -n 50 --no-pager | grep -F '[Diag]'   # строка «diagnostic log in …» при старте
```

Выключить: `rm /etc/systemd/system/mihomo.service.d/diag.conf && systemctl daemon-reload && systemctl restart mihomo`.

## Убрать mihomo-monitor (после того как журнал заработал)

Точных имён его файлов у меня нет — сначала посмотреть:

```
systemctl list-timers --all | grep -i monitor
systemctl cat mihomo-monitor.timer mihomo-monitor.service   # путь к скрипту — в ExecStart
```

Потом (подставив настоящие имена и путь к скрипту из ExecStart):

```
systemctl disable --now mihomo-monitor.timer mihomo-monitor.service
rm -f /etc/systemd/system/mihomo-monitor.timer /etc/systemd/system/mihomo-monitor.service
rm -f /путь/к/скрипту/из/ExecStart
systemctl daemon-reload && systemctl reset-failed
```

## Собрать журнал за последние двое суток (на хосте Proxmox)

```
pct exec 101 -- sh -c '
T=$(mktemp -d)
cd /var/log/mihomo-diag && cp $(ls -1 diag-*.log* | tail -n 3) "$T"/
journalctl -u mihomo --since "48 hours ago" -o short-iso --no-pager | grep -E "systemd\[1\]|panic|fatal error|goroutine [0-9]+ \[" > "$T/journal-lifecycle.txt"
/usr/local/bin/mihomo -v > "$T/version.txt" 2>&1
tar -C "$T" -czf - . ; rm -rf "$T"' > /root/mihomo-diag-$(date +%Y%m%d-%H%M).tar.gz
ls -la /root/mihomo-diag-*.tar.gz
```

* В архив попадают три последних дневных файла — сегодня и два полных предыдущих дня (то есть не меньше
  48 часов), из журнала systemd — только строки о запусках/остановках службы и о падениях (panic), и
  версия mihomo. Обычный журнал mihomo с соединениями гостей в архив **не** попадает.
* Забрать на компьютер: `scp root@10.0.9.1:/root/mihomo-diag-*.tar.gz .` — и прислать файл.

## Что в журнале

Каждая строка: `время событие поле=значение …`, время местное с миллисекундами и поясом. Имена — в
кавычках. `-` вместо задержки — результата нет.

| событие | когда | основное |
|---|---|---|
| `start` | запуск mihomo | версия, Go, pid, период |
| `config` | запуск и каждая перезагрузка конфига | группы (url, tolerance, max-failed, timeout, участники), провайдеры (url проверки, interval, timeout, lazy, extra-URL с фильтрами) — без ссылки подписки |
| `fetch` | каждое скачивание подписки | `result=same/updated/error`, ошибка с маской |
| `provider-update` | новый список узлов | версия, число узлов, какие имена добавились/пропали |
| `hc-start` / `hc-end` | заход health-check | провайдер (или fallback-группа), `round`, причина: `start`/`tick`/`update`/`request`, число узлов, длительность, сколько живых |
| `hc-node` | каждый узел в заходе | `ok`, показанная задержка `delay`, полная длительность проверки `full`, ошибка |
| `hc-skip` | заход не запущен | присоединился к идущему (или <1 с назад) — так выглядела потерянная проверка |
| `hc-recheck` | обновление посреди захода | запущен новый заход по новому списку (правка №2) |
| `url-test` | url-test сменил узел (или начал/кончил «ждать проверки») | `from`, `to`, причина: `first`, `removed`, `dead`, `slower`, `last-result-dead`, `pending`, `keep`, `fixed`; задержки, лучший узел, tolerance |
| `fallback` | fallback сменил участника | откуда, куда, состояния всех участников |
| `max-failed` | сработал `max-failed-times` | группа, число неудач, окно |
| `api` | действия через API/панель | проверка задержки группы/узла (с тайм-аутом панели!), ручной выбор, снятие фиксации, обновление/проверка провайдера |
| `status` | раз в минуту на группу | url-test: текущий, лучший, Δ, alive/dead/untested, `fixed`, давность решения, соединения за минуту (`dial_ok`/`dial_fail`); fallback: цепочка до узла с задержкой, состояния участников |
| `warn` | Δ > tolerance | то, что mihomo-monitor писал как «ВНИМАНИЕ» |
| `runtime` | раз в минуту | аптайм, горутины, память (heap/sys/RSS), число соединений, трафик всего |

Пример (из локальной имитации, сценарий S5: старт, смерть узла, `max-failed-times`, fallback; по одной-две строки каждого вида, не по порядку времени; время там в UTC, в контейнере будет местное). Строки `status`/`warn` с `COMPATIBLE` — то самое окно после запуска, когда группа ходит напрямую:

```
2026-10-22T18:34:59.911Z start version=sim-v4b build="unknown time" go=go1.26.10 pid=8186 interval=5s
2026-10-22T18:34:59.971Z config url-test="UT" url=http://127.0.0.1:18204/generate_204 expected="*" tolerance=50 max_failed=3 timeout=5000 providers=["sim"] members=3
2026-10-22T18:34:59.971Z config provider="sim" vehicle=File hc_url=http://127.0.0.1:18204/generate_204 interval=30s timeout=2s lazy=false expected="204" extra=[] proxies=5
2026-10-22T18:34:59.923Z provider-update provider="sim" version=1 proxies=5 initial
2026-10-22T18:34:59.923Z hc-start provider="sim" round=8c50b834 reason=update proxies=5 version=1 url=http://127.0.0.1:18204/generate_204 extra=[]
2026-10-22T18:35:00.047Z hc-node provider="sim" round=8c50b834 node="n2" ok=true delay=61 full=124
2026-10-22T18:35:00.229Z hc-end provider="sim" round=8c50b834 reason=update tests=5 alive=5 dur=305
2026-10-22T18:35:09.972Z status url-test="UT" now="n2" delay=61 best="n2" best_delay=61 delta=0 tol=50 alive=3 dead=0 untested=0 total=3 fixed="" decided=0s reason=loaded dial_ok=1 dial_fail=0
2026-10-22T18:35:09.971Z status fallback="FB" now="UT" chain="UT → n2" leaf_delay=61 fixed="" members=["UT":1 "BK":1] dial_ok=0 dial_fail=0
2026-10-22T18:35:04.973Z runtime uptime=5s goroutines=12 heap=3MiB sys=17MiB rss=44MiB gc=2 conns=0 up=0MiB down=0MiB
2026-10-22T18:35:04.973Z status fallback="FB" now="UT" chain="UT → COMPATIBLE" leaf_delay=- fixed="" members=["UT":1 "BK":1] dial_ok=0 dial_fail=0
2026-10-22T18:35:04.973Z status url-test="UT" now="COMPATIBLE" delay=- best="n2" best_delay=61 delta=- tol=50 alive=3 dead=0 untested=0 total=3 fixed="" decided=5s reason=first dial_ok=1 dial_fail=0 empty_fallback=true
2026-10-22T18:35:04.973Z warn url-test UT hands out its empty fallback "COMPATIBLE" while 3 of its proxies are alive (decided 5s ago)
2026-10-22T18:35:09.924Z url-test group="UT" from="COMPATIBLE" to="n2" reason=loaded from_delay=- to_delay=61 best="n2" best_delay=61 tol=50
2026-10-22T18:35:40.099Z url-test group="UT" from="n2" to="n3" reason=dead from_delay=- to_delay=101 best="n3" best_delay=101 tol=50
2026-10-22T18:35:50.251Z max-failed group="UT" failures=3 window=5000ms failed multiple times: health check of the group's providers (0 more in the previous 10 s+ not logged)
2026-10-22T18:35:24.972Z fallback group="FB" from="UT" to="BK" fixed="" members=["UT":dead "BK":81]
```
