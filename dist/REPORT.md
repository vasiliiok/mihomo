# mihomo v1.19.33-urltestfix3 — отчёт о проверке и сборке

Дата: 22.10.2026. Ветка форка: `claude/brave-fermi-zen3ue` (`vasiliiok/mihomo`).

## 0. Коротко

* Бинарник: `dist/mihomo-linux-amd64-v1-v1.19.33-urltestfix3.gz` (linux/amd64, GOAMD64=v1,
  статический, Go 1.26.10 — тот же, что у официального v1.19.33). Контрольные суммы — в `dist/SHA256SUMS`
  и в разделе 6.
* Внутри — v1.19.33 и **три** коммита:
  1. `7b870437` fix(url-test): follow proxies recreated by provider updates — присланный коммит 1 (в проде);
  2. `4c533ed4` fix(url-test, health-check): keep recreated nodes and test them promptly — присланный коммит 2;
  3. `9d72bf65` fix(url-test): judge a recreated node by its previous result — **новый**, исправляет
     регрессию, которую вносил коммит 2 (раздел 3.1).
* Присланные 4 теста на настоящем коде v1.19.33 падают, с патчем проходят; `go vet`, `-race` и полный
  `go test ./...` (с `with_gvisor` и без) — чисто.
* Правило коммита 2 «держать текущий узел, пока у него нет результата» оказалось ошибочным: оно держит
  **навсегда** узел, который никогда не проверяется по URL группы, и держит пересозданный узел, чей
  последний результат — «мёртв». На чистом v1.19.33 оба случая работают правильно, то есть это регрессия
  патча. Заменил на «пока у нового объекта нет результата, судить по последнему результату старого объекта
  того же узла». Подтверждено модульными тестами и интеграционным сценарием S6.
* В upstream эта же ошибка url-test известна: issue #3268 (закрыт как дубликат #2945), PR #3232 (закрыт
  автором) и #3269 (открыт, без ревью). У них то же «держать, пока 0xffff», что и в коммите 2, — с той же
  дырой; мейнтейнер в #3232 задал как раз этот вопрос. Про потерю проверки при обновлении (health-check) в
  upstream ничего нет.

## 1. Подготовка и сверка исходных данных

| что | результат |
|---|---|
| Форк | `vasiliiok/mihomo` — форк MetaCubeX/mihomo. Ветка по умолчанию у него `main` с посторонней историей (Python-библиотека для Honkai: Star Rail), на неё не смотреть. `Alpha` = `Meta` = тег `v1.19.33` = `3f27ef39f8c96bbfbc8b0b9bb10f5c9ef60149d8` — сверено и в форке, и в upstream (`git ls-remote`). |
| Изменения upstream после v1.19.33 | upstream `Alpha` указывает ровно на `v1.19.33`, изменений в `adapter/outboundgroup`, `adapter/provider`, `adapter/adapter.go`, `common/singledo`, `component/resource` нет. |
| Версия Go | официальный бинарник v1.19.33 (`mihomo-linux-amd64-v1-v1.19.33.gz` из релиза) по `go version -m` собран **go1.26.10**; на go.dev последняя ветки 1.26 — 1.26.10 (есть ещё 1.27.3, но upstream собирает на 1.26). Workflow берёт Go из `MetaCubeX/go` (`go-version: '1.26'`); их патчи поверх Go касаются только Windows 7 и macOS, для linux/amd64 это обычный Go. Я собирал `GOTOOLCHAIN=go1.26.10` (тулчейн с proxy.golang.org). |
| Патч | sha256 `fd5e8095…482a` совпал; `git am` применился; sha256 всех пяти файлов после применения совпали с указанными. |
| Ветка | в задании `urltestfix2`, но сессия разрешает пушить только в `claude/brave-fermi-zen3ue`, поэтому всё там. Ветка до этого совпадала с посторонней `main` — переставлена на v1.19.33. |

## 2. Модульные тесты «до/после» на настоящем коде

Тестовые файлы — итоговые (4 присланных теста + 2 новых из коммита 3), код — на четырёх точках:
`clean` = v1.19.33, `c1` = только коммит 1 (как в проде), `v2` = присланный патч, `v3` = итог.

| тест | clean | c1 | v2 | v3 |
|---|---|---|---|---|
| TestURLTestFollowsRecreatedProviderProxies | FAIL | ok | ok | ok |
| TestURLTestKeepsRecreatedProxyUntilItIsTested | FAIL | FAIL | ok | ok |
| TestURLTestToleranceAppliesToFirstProxy | FAIL | FAIL | ok | ok |
| TestHealthCheckTestsProxiesReplacedDuringACheck | FAIL | FAIL | ok | ok |
| TestURLTestLeavesProxyThatIsNeverTested (новый) | ok | ok | **FAIL** | ok |
| TestURLTestLeavesRecreatedProxyThatWasDead (новый) | ok | ok | **FAIL** | ok |

Сообщения падений (дословно из `go test -v`):

```
clean: url-test returned a proxy object the provider has already replaced
clean, c1: url-test left its node before any proxy was tested
clean, c1: tolerance was ignored because the selected node is the first one
clean, c1: proxies set during a running health check were not tested
v2: url-test stayed on a proxy that has never been tested
v2: url-test kept a recreated proxy whose last result was a failure
```

Остальные проверки (на v2 и повторно на v3):

* `go vet ./adapter/... ./common/singledo/... ./component/resource/...` — чисто;
* `go test -race` тех же пакетов — ok; новые тесты `-race -count=50` — ok;
* `SKIP_CONCURRENT_TEST=1 SKIP_INTEROP_TEST=1 go test ./...` и то же с `-tags with_gvisor`, как в CI —
  116 пакетов ok, падений нет (сравнивать с чистым тегом не понадобилось);
* `gofmt -l` показывает только `component/memory/memory_freebsd*.go` — они такие и в теге, не трогал.

## 3. Ревью

### 3.1. Ошибка в коммите 2 (исправлена коммитом 3)

Правило коммита 2: если у текущего объекта узла нет результата для `testUrl`, а сам он не помечен мёртвым,
— держать его. Но «нет результата» бывает не только сразу после обновления подписки:

1. **Узел, который никогда не проверяется по URL группы.** Группа выбирает первый узел при самом первом
   вызове — до любых проверок. Если именно этот узел по URL группы не проверяется никогда, коммит 2 держит
   его вечно. Штатный пример (сценарий S6): две группы с одинаковым нестандартным `url`, у одной `filter`,
   у другой нет. `registerHealthCheckTask` объединяет их в одну extra-задачу: непустые фильтры групп
   складываются, а группа без фильтра фильтр **не снимает** (особенность upstream), и узлы второй группы
   вне фильтра по этому URL не проверяются никогда. Upstream в такой ситуации уходит на проверенный узел,
   коммит 2 — нет.
2. **Пересозданный узел, последний результат которого — «мёртв».** Новый объект без проверок считается
   живым (`alive=true` по умолчанию), и коммит 2 держал его, хотя старый объект того же узла уже упал.

Исправление (коммит 3, `adapter/outboundgroup/urltest.go`): пока у текущего объекта нет результата для
`testUrl`, его оценивают по результату **предыдущего объекта** того же узла (он остаётся в `fastNode` как
опорный, наружу отдаётся текущий объект). Жив и в пределах tolerance — остаёмся, мёртв или хуже — уходим.
Если и у предыдущего результата нет — обычное сравнение (0xffff), как в upstream, но уже в `int`.
Присланные тесты проходят без изменений, к ним добавлены два теста на случаи выше.

Цена: старый объект узла живёт в памяти, пока новый не проверен (обычно секунды). Если авто-проверок нет
вовсе, — дольше; upstream с ошибкой №1 держал его часами.

### 3.2. Ответы на вопросы задания

**Может ли удержание непроверенного узла надолго оставить группу на мёртвом узле?**
С коммитом 3 — только если одновременно: (а) предыдущий объект узла был жив и не хуже tolerance, (б) новый
объект не проверяется, (в) его общий `alive` (результат проверки любым URL) не упал. По случаям:

* *health-check провайдера выключен.* Авто-проверок нет совсем; после обновления группа остаётся на том же
  имени (upstream прыгал бы на `proxies[0]`, тоже не проверенный). Если узел мёртв, соединения падают →
  `max-failed-times` → `provider.HealthCheck()` → проверка → уход. Если у провайдера и `url` пуст, проверять
  нечем ни в одной версии.
* *`url` группы отличается от провайдера.* Он регистрируется extra-задачей и проверяется тем же заходом по
  таймеру провайдера, `recheck()` запускает и его. Задержка — до конца одного захода. Тонкость: если у
  провайдера проверки выключены, а у группы `interval` не задан, `interval` остаётся 0 — авто-проверок нет
  (так в upstream).
* *`filter` у extra-задачи.* Узлы группы проверяются, если подходят под объединённый фильтр. Исключение —
  случай п. 3.1.1: коммит 2 держал бы такой узел вечно, коммит 3 — нет (оценки по старому объекту нет →
  обычное сравнение).
* *`lazy: true`.* Проверка после обновления (`recheck`) запускается независимо от `lazy`, как и в upstream.

**Корректна ли семантика `recheck()`?** Да, с одной безвредной гонкой.

* `singledo.Reset()` не останавливает идущую проверку: старая доходит до конца по старому срезу (результаты
  пишутся в старые объекты), новая параллельно проверяет новый срез. До 2×10 одновременных проверок на
  время одного захода. Постоянного задвоения быть не может: каждая проверка одноразовая, плановый тик
  присоединяется к текущему вызову `singledo`.
* Одновременные `recheck()` (два обновления подряд) и старт: если `recheck` прочитал «устарело» до того,
  как параллельная проверка сделала снимок того же нового среза, обе запустятся — одна лишняя проверка
  одного и того же списка. На старте (`process()` → `go check()` и `Fetcher.Initial` → `setProxies` →
  `recheck`) это возможно, но маловероятно: `check()` видит пустой список и выходит, пока файл не прочитан.
  В интеграционных логах на старте лишних заходов не видно (раздел 4). Убрать окно полностью можно только
  меняя `singledo` — не стал.
* В upstream было и второе проявление той же ошибки: если предыдущая проверка закончилась меньше секунды
  назад, `go check()` возвращал её кэш и новый список тоже не проверялся. `recheck` это покрывает.

**Есть ли в upstream issue/PR?** (искал веб-поиском, ничего туда не писал)

* [#3268](https://github.com/MetaCubeX/mihomo/issues/3268) — ровно ошибка №1 (url-test держит заменённый
  объект). Закрыт 03.10 xishang0128 как дубликат [#2945](https://github.com/MetaCubeX/mihomo/issues/2945),
  метки «invalid», «not following template».
* [#3232](https://github.com/MetaCubeX/mihomo/pull/3232) (закрыт автором, не влит) и
  [#3269](https://github.com/MetaCubeX/mihomo/pull/3269) (открыт с 03.10, ревью нет; есть подтверждение
  пользователя на v1.19.32): поиск по всем узлам с перепривязкой по имени, «держать, пока задержка 0xffff»,
  tolerance в `uint32`. Health-check не трогают. Мейнтейнер wwqgtxx в #3232 спросил: «а если новый узел
  так и не подключится — url-test никогда не переключится?» — ответа не было; дыра п. 3.1.1 как раз из
  этой области.
* [#3336](https://github.com/MetaCubeX/mihomo/issues/3336) (закрыт без разбора): зависшая проверка
  (MASQUE игнорирует тайм-аут) держит `singledo` → у провайдера больше нет проверок. Наш патч этого не
  лечит; при обновлении подписки `recheck` хотя бы запускает свежий заход.

### 3.3. Проверено, ошибок того же класса нет

* fallback, selector, `fixed` url-test хранят имя, а не объект; load-balance — индексы;
* кэш `GroupBase.GetProxies` сверяет версии провайдеров;
* `Fetcher` вызывает `onUpdate` только при смене хэша; file-провайдер — через fswatch;
* `autoCloseProxyAdapter` закрывает устаревший адаптер только финализатором, соединения держат ссылку;
* `uint16`-арифметики, кроме исправленной, в `adapter/`, `hub/`, `tunnel/`, `component/` нет;
* порядок блокировок `bp.mutex → hc.mu` нигде не инвертируется.

### 3.4. Найдено, но не исправлено

* **Кэш `fastSingle` 10 с.** До 10 с после обновления группа отдаёт старый объект. Измерено в S1: 10–13
  соединений на старый порт за первые 12 с, потом ни одного. Старый объект работоспособен (закрывается
  только финализатором), неудача соединения запускает `max-failed-times` — не стоит отдельной правки.
* **Особенность extra-фильтров** (п. 3.1.1): узлы группы без фильтра по общему URL могут не проверяться
  никогда. Это upstream, не наш случай (все группы на URL провайдера).
* **fallback с `use:`**: новые объекты без проверок считаются живыми, поэтому сразу после обновления
  fallback может выбрать узел, который был мёртв, — до его проверки (с `recheck` это секунды). У тебя
  fallback-группы на `proxies:`, не задевает.
* **Старт: `COMPATIBLE` на 10 с.** Если url-test спросили до загрузки провайдера, он кэширует `COMPATIBLE`
  на 10 с (S5, во всех сборках одинаково). Это upstream.
* Гонки без блокировки `fastNode`/`selected` (если `fastSingle.Reset()` случится посреди вычисления) и
  `GroupBase.failedTimes` — upstream, не трогал.

## 4. Интеграционные сценарии на настоящих бинарниках

РЕЗУЛЬТАТЫ — см. ниже (заполняется по итогам прогона).

## 5. Сборка

Собрано из чистого клона на коммите `9d72bf65` (в worktree Go не ставит VCS-метку), флаги — как в
`.github/workflows/build.yml` тега (`BUILDTAG=-extldflags --static`, `CGO_ENABLED=0`, `-tags with_gvisor`,
`-trimpath`, `-w -s -buildid=`):

```
GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0 GOTOOLCHAIN=go1.26.10 \
go build -tags "with_gvisor" -trimpath -ldflags "-extldflags --static \
  -X 'github.com/metacubex/mihomo/constant.Version=v1.19.33-urltestfix3' \
  -X 'github.com/metacubex/mihomo/constant.BuildTime=Thu Oct 22 16:52:17 UTC 2026' -w -s -buildid=" \
  -o mihomo-linux-amd64-v1-v1.19.33-urltestfix3 .
```

Проверки:

```
$ ./mihomo-linux-amd64-v1-v1.19.33-urltestfix3 -v
Mihomo Meta v1.19.33-urltestfix3 linux amd64 with go1.26.10 Thu Oct 22 16:52:17 UTC 2026
Use tags: with_gvisor

$ file mihomo-linux-amd64-v1-v1.19.33-urltestfix3
ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked, stripped

$ go version -m mihomo-linux-amd64-v1-v1.19.33-urltestfix3   (без списка зависимостей)
mihomo-linux-amd64-v1-v1.19.33-urltestfix3: go1.26.10
	build	-buildmode=exe
	build	-compiler=gc
	build	-tags=with_gvisor
	build	-trimpath=true
	build	CGO_ENABLED=0
	build	GOARCH=amd64
	build	GOOS=linux
	build	GOAMD64=v1
	build	vcs=git
	build	vcs.revision=9d72bf65320ffb852d5861242a8df2f3bb50552a
	build	vcs.modified=false
```

Сборка воспроизводима: повторная полная пересборка (`go build -a`) того же коммита той же командой
(BuildTime задан строкой выше) дала тот же sha256 `5e5a7752…c735` — любой может пересобрать и сверить.

Сравнение с официальным `mihomo-linux-amd64-v1-v1.19.33`: та же версия Go (go1.26.10), те же
`-tags`, `CGO_ENABLED`, `GOAMD64`, `-trimpath`; строка `DefaultGODEBUG` совпадает полностью. Отличия:
`vcs.revision` (наш коммит), `vcs.modified=false` (у официального `true` — из-за скопированного CA-bundle)
и пустой встроенный CA-bundle (ниже).

**Встроенные корневые сертификаты не заполнены.** Официальный CI перед сборкой копирует системный
`/etc/ssl/certs/ca-certificates.crt` в `component/ca/ca-certificates.crt`. Здесь это сделать нельзя: в
облачной машине в системном наборе может быть CA прокси песочницы, а зашивать такой CA в бинарник шлюза
нельзя. Файл оставлен пустым, как в теге (0 байт). mihomo тогда использует системные сертификаты LXC
(`x509.SystemCertPool()`) — в Debian это пакет `ca-certificates`. На практике: держать `ca-certificates` в
контейнере установленным и обновлённым (`apt install ca-certificates`). Отсюда и разница в размере:
.gz 22 787 834 байт против 22 893 964 у официального.

`-t` на конфиге с группами из раздела 1 (`dist/integration/user-config/`, провайдер заменён локальным
файлом с похожими именами узлов): `configuration file … test is successful` — и у этой сборки, и у
официальной v1.19.33.

Короткий запуск на том же конфиге (URL проверки заменён на локальный 204, узлы — локальные SOCKS5 из
`sim.py`, правило `MATCH,NON-RU-FIRST`): три запроса через mixed-port — `HTTP 204` за 0,04–0,06 с;
в логе ни одной строки `error`/`fatal`/`warning`; группы выбрали:

```
PROXY-NON-RU           now='🇫🇮 Финляндия ⚡⭐ LTE'   (40 мс — самый быстрый из 4)
PROXY-RU               now='🇷🇺 Россия LTE'
PROXY-YOUTUBE          now='🇳🇱 Нидерланды ⚡ LTE'
PROXY-GEMINI           now='🇩🇪 Германия ⭐ LTE'
PROXY-YOUTUBE-GEMINI   now='🇫🇮 Финляндия ⚡⭐ LTE'
NON-RU-FIRST → PROXY-NON-RU, RU-FIRST → PROXY-RU, YOUTUBE и GOOGLE → PROXY-YOUTUBE-GEMINI
```

(узел «🇺🇸 США» отсеян `filter` провайдера, как и должно быть).

## 6. Установка

Файл: `mihomo-linux-amd64-v1-v1.19.33-urltestfix3.gz`

```
sha256 (.gz):          1cf85c7ecd40203b715a4ba467ff826ddb598690342ac502e8ba6d0cdffbe1f1
sha256 (распакованный): 5e5a7752a27d4153a49fd3c41d78eabedb4a0476eee7c4ec29409135942bc735
```

```
# на компьютере: скачать .gz, затем
scp mihomo-linux-amd64-v1-v1.19.33-urltestfix3.gz root@10.0.9.1:/root/
# на хосте Proxmox
cd /root && gunzip -k mihomo-linux-amd64-v1-v1.19.33-urltestfix3.gz && sha256sum mihomo-linux-amd64-v1-v1.19.33-urltestfix3
#   должно быть 5e5a7752a27d4153a49fd3c41d78eabedb4a0476eee7c4ec29409135942bc735
pct push 101 /root/mihomo-linux-amd64-v1-v1.19.33-urltestfix3 /usr/local/bin/mihomo.new --perms 0755
pct enter 101
# в контейнере
/usr/local/bin/mihomo.new -v
/usr/local/bin/mihomo.new -d /etc/mihomo -t
cp -p /usr/local/bin/mihomo /usr/local/bin/mihomo.urltestfix1
mv -f /usr/local/bin/mihomo.new /usr/local/bin/mihomo
systemctl restart mihomo && systemctl status mihomo --no-pager -n 20 && mihomo -v
```

* Откат: `cp -p /usr/local/bin/mihomo.urltestfix1 /usr/local/bin/mihomo.tmp && mv -f /usr/local/bin/mihomo.tmp /usr/local/bin/mihomo && systemctl restart mihomo`
  (поверх работающего файла писать нельзя — Text file busy, поэтому копия + `mv`).
* После перезапуска fallback-группы до 1 минуты могут стоять на запасных (окно после запуска, interval 60) — это нормально.
* Кнопку обновления ядра в панели не нажимать: она поставит официальную сборку без правок.
* Что смотреть в `mihomo-monitor` после установки: переключений url-test в :03 (минута после обновления
  подписки) должно стать заметно меньше; Δ по-прежнему не должен превышать tolerance.

## 7. Файлы

* `dist/mihomo-linux-amd64-v1-v1.19.33-urltestfix3.gz`, `dist/SHA256SUMS` — сборка;
* `dist/integration/sim.py` — имитация узлов (SOCKS5 с управляемой задержкой/режимом) и цели 204;
* `dist/integration/scenarios.py` — сценарии S1–S6; запуск: `python3 scenarios.py <mihomo> <метка> <каталог>`;
* `dist/integration/results/` — итоговые `summary-*.json` и `events.txt` каждого прогона;
* `dist/integration/user-config/` — конфиг для `-t` с группами из прода.
