# cephsim

Симулятор «что будет, если…» для Ceph. Работает на снимке кластера и в кластере ничего не меняет. Отвечает на вопросы до начала работ:

- можно ли выключить хост, стойку или ряд, и сколько PG станут inactive;
- сколько данных переедет и сколько это займёт с учётом клиентской нагрузки;
- не переполнятся ли OSD;
- в каком порядке и какими партиями обслуживать хосты;
- что будет после добавления хостов, смены CRUSH-правила или `pg_num`.

Раскладку PG считает собственный порт CRUSH и логики OSDMap на Go. На проверенных рабочих кластерах она совпадает с `osdmaptool` и с `ceph pg ls` по каждой PG.

## Состояние

| Что | Статус |
|-----|--------|
| Раскладка PG (CRUSH, upmap, reweight) | совпадает один в один на двух рабочих кластерах (4 513 PG; 19 649 PG с 8 251 `pg_upmap_items`) и на синтетике (4 608 PG) |
| Порт CRUSH против `crushtool --test` | 25 проверок по 4 096 входов, все совпадают (Ceph v20) |
| Время recovery | один back-test на реальном восстановлении: факт 17,7 ч, модель 16,0 ч |
| Replicated-пулы | проверены на реальных кластерах (Ceph 17 и 20) |
| **EC-пулы** | **на кластерах, где проверялся cephsim, EC-пулов не было.** Логика EC (шард 1/k, incomplete при < k живых шардов, indep- и MSR-правила) проверена только юнит-тестами и `crushtool` на синтетике, на реальных данных - нет. См. [docs/accuracy.md](docs/accuracy.md#ec) |

## Быстрый старт

```bash
go install github.com/timofey-maykov/cephsim@latest   # или из клона: go build -o cephsim .

# 1. снимок кластера (на admin-ноде, только команды чтения)
./cephsim collect -o snap-prod -name PROD -ceph "sudo ceph"

# 2. один раз на кластер: сверить раскладку со штатным osdmaptool
./cephsim verify -snap snap-prod

# 3. скорость recovery и клиентская нагрузка по истории VictoriaMetrics
./cephsim calibrate -snap snap-prod -prometheus http://victoria-metrics:8428/select/0/prometheus \
  -selector 'cluster="PROD"' -since 30d -backfills 1

# 4. сценарии
./cephsim simulate -snap snap-prod -action maintenance -target host-07 -noout -window 3h -compare
./cephsim simulate -snap snap-prod -action rolling -window 2h -assume-clean

# 5. отчёт для браузера: один HTML-файл, можно приложить к заявке на работы
./cephsim simulate -snap snap-prod -action drain -target host-07 -compare -format html > drain-host-07.html

# 6. карта рисков всего кластера: отказ и обслуживание каждого хоста, стойки, ряда
./cephsim report -snap snap-prod > prod-cluster.html
```

Примеры HTML-отчётов на синтетическом кластере лежат в `reports/`: `cluster.html` - карта рисков, остальные - отдельные сценарии.

Если на admin-ноде нет cephsim, снимок можно снять с рабочей машины через `ssh`, см. [docs/commands.md](docs/commands.md#снимок-без-cephsim-на-кластере).

## Сценарии

| `-action` | Что моделирует |
|-----------|----------------|
| `maintenance` | хост выключается на окно работ и возвращается, с `-noout` или без |
| `failure` | внезапная потеря хоста, стойки, ряда или OSD |
| `drain` | вывод навсегда: CRUSH-вес 0, данные уезжают с живых OSD |
| `expand` | новые хосты с OSD |
| `rule` | пул переводится на другое или новое CRUSH-правило |
| `pgnum` | новый `pg_num` пула |
| `rolling` | обслуживание хостов по очереди: партии и расписание |

Подробно - в [docs/scenarios.md](docs/scenarios.md).

## Документация

| Документ | О чём |
|----------|-------|
| [docs/commands.md](docs/commands.md) | все команды и флаги, файлы снимка, JSON- и HTML-вывод |
| [docs/scenarios.md](docs/scenarios.md) | каждый сценарий: что делает, флаги, примеры, как читать результат |
| [docs/model.md](docs/model.md) | как считаются доступность, переезд, заполненность, время и вердикт |
| [docs/calibration.md](docs/calibration.md) | откуда берутся скорость слота и клиентская нагрузка, модель mClock |
| [docs/accuracy.md](docs/accuracy.md) | как проверена точность, что не проверено, ограничения |
| [docs/development.md](docs/development.md) | устройство кода, тесты, фикстуры |

## Лицензия

MIT, см. [LICENSE](LICENSE).
