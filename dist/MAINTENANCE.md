# Обновление сборки на новый релиз mihomo

Порядок для каждой новой версии upstream (MetaCubeX/mihomo). Ветка форка — `claude/brave-fermi-zen3ue`
(`vasiliiok/mihomo`); новые версии вливаются в неё **merge** тега, без переписывания истории, чтобы ссылки
не ломались.

## Наши правки (на v1.19.33)

| коммит | что | можно выбросить, когда |
|---|---|---|
| `7b870437` fix(url-test): follow proxies recreated by provider updates | url-test ищет текущий объект узла по имени | upstream примет эквивалент (PR #3269 / issue #2945, #3268) |
| `4c533ed4` fix(url-test, health-check): keep recreated nodes and test them promptly | `int` в tolerance, первый узел, `recheck()` в health-check, гонка `hc.proxies` | для url-test — вместе с предыдущим; для health-check — если upstream начнёт проверять новый список при обновлении посреди захода |
| `9d72bf65` fix(url-test): judge a recreated node by its previous result | оценка по старому объекту вместо «держать, пока не проверен» | вместе с url-test-правками |
| diag (urltestfix4) | диагностический журнал `MIHOMO_DIAG_DIR` (`component/diag`, `hub/diagreport`, хуки) | наш инструмент, оставляем |
| `c8c4d5ed` fix(provider): keep the health check history of unchanged nodes on update | новый объект узла получает историю старого (те же имя, тип, адрес) — `adapter.Proxy.InheritHealth`, `provider.inheritHealth` | upstream перестанет обнулять историю при обновлении подписки |
| `4897c063` feat(url-test): reliability and delay-samples options | параметры групп `reliability`, `delay-samples` (`rate`/`better` в `urltest.go`) | upstream добавит учёт надёжности/сглаживание; **в конфиге параметры останутся, официальная сборка их молча игнорирует** |
| `48e3610b` feat(fallback): fail-confirm option | параметр `fail-confirm`: участник мёртв только после проваленной повторной проверки | upstream добавит подтверждение отказа; то же про конфиг |
| `01015d7f` fix(diag): count connections after the handshake, health checks apart | счётчики журнала: `dial_ok` после рукопожатия, `hc_ok`/`hc_fail`, `alloc`, `new_conns` (`statistic.Manager.Joined`) | часть журнала, оставляем |

Регрессионные тесты правок: `adapter/outboundgroup/urltest_test.go`, `adapter/outboundgroup/fallback_test.go`,
`adapter/provider/healthcheck_test.go`, `adapter/adapter_test.go`, `*/diag_test.go`; интеграционные сценарии —
`dist/integration/` (S1–S9).

При слиянии нового тега особенно смотреть на изменения в `adapter/adapter.go` (история проверок,
`URLTest`), `adapter/outboundgroup/{urltest,fallback,groupbase}.go` и `adapter/provider/provider.go`
(`setProxies`): наши правки сидят именно там. `go.mod` объявляет `go 1.20` — не использовать `min`/`max`,
`slices`/`maps` из стандартной библиотеки (upstream собирает и под старые Go).

## Шаги

1. **Что вышло.** Релиз и changelog: `https://github.com/MetaCubeX/mihomo/releases/tag/vX.Y.Z`; коммиты
   между тегами, особенно в `adapter/outboundgroup`, `adapter/provider`, `adapter/adapter.go`,
   `common/singledo`, `component/resource`, `hub/route`, `hub/executor`:
   `git log --oneline vOLD..vNEW -- <каталоги>` и `git diff vOLD vNEW -- <каталоги>`.
   Проверить, не влиты ли PR #3269 или другой фикс тех же ошибок, открытые issue про url-test/health-check.
2. **Тег и Go.** `git ls-remote https://github.com/MetaCubeX/mihomo.git refs/tags/vX.Y.Z`; скачать
   официальный `mihomo-linux-amd64-v1-vX.Y.Z.gz` из релиза и посмотреть `go version -m` — это версия Go для
   нашей сборки; сверить с последним патч-релизом ветки на `https://go.dev/dl/?mode=json`. Флаги — из
   `.github/workflows/build.yml` тега.
3. **Влить.** `git fetch origin 'refs/tags/vX.Y.Z:refs/tags/vX.Y.Z'` (если в форке тега нет — из upstream),
   `git merge vX.Y.Z`, конфликты решать вручную. Если upstream починил что-то из нашего — убрать
   соответствующую правку отдельным коммитом (`git revert` или правка), объяснив в сообщении.
4. **Проверить.** `GOTOOLCHAIN=goX.Y.Z`:
   * новые/наши тесты «до/после»: на чистом теге с нашими тестовыми файлами — какие падают, с правками —
     все проходят;
   * `go vet` и `go test -race` для `./adapter/... ./common/singledo/... ./component/... ./hub/... ./tunnel/...`
     (весь `./...` под `-race` не укладывается в 10 минут на `listener/inbound` — тяжёлые тесты слушателей);
   * `SKIP_CONCURRENT_TEST=1 SKIP_INTEROP_TEST=1 go test ./...` и то же с `-tags with_gvisor`;
   * интеграция: собрать `clean` (чистый тег) и новую сборку, `python3 dist/integration/scenarios.py
     <бинарник> <метка> <каталог>` для обоих (и с `SIM_DIAG=1` для новой); ожидания — таблица в REPORT.md.
5. **Собрать** из чистого клона (в worktree Go не ставит VCS-метку), без встроенного CA-bundle:
   ```
   GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0 GOTOOLCHAIN=goX.Y.Z \
   go build -tags "with_gvisor" -trimpath -ldflags "-extldflags --static \
     -X 'github.com/metacubex/mihomo/constant.Version=vX.Y.Z-urltestfixN' \
     -X 'github.com/metacubex/mihomo/constant.BuildTime=$(LC_ALL=C date -u)' -w -s -buildid=" \
     -o mihomo-linux-amd64-v1-vX.Y.Z-urltestfixN .
   ```
   Проверить `-v`, `file`, `go version -m`, `-t -d dist/integration/user-config`, короткий запуск.
   Номер `N` — номер набора правок (urltestfix4 = правки + журнал, urltestfix5 = + надёжность url-test,
   fail-confirm, история при обновлении); меняется, только когда меняются правки.
6. **Выдать.** В `dist/` — новый `.gz` (старый удалить: он останется в истории git), `SHA256SUMS`, отчёт;
   push; ссылки на файл и diff, sha256 и инструкция по установке пользователю.
