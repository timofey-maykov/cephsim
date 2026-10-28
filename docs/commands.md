# Команды

```
cephsim collect   -o DIR                       снять снимок кластера
cephsim verify    -snap DIR                    сверить раскладку с osdmaptool и pg ls
cephsim calibrate -snap DIR -prometheus URL    скорость recovery и клиентская нагрузка по истории
cephsim simulate  -snap DIR -action A [флаги]  посчитать сценарий
cephsim report    -snap DIR > cluster.html     карта рисков всего кластера
```

Сборка: `go build -o cephsim .` (Go 1.22+). Бинарник один, внешних зависимостей нет. Для `verify` нужен `osdmaptool`: в `PATH`, в каталоге из `-ceph-bin` или в docker-образе Ceph.

## collect

Снимает снимок: набор файлов, по которым дальше считается всё остальное. Запускается там, где работает `ceph` CLI с правами admin.

| Флаг | По умолчанию | Что |
|------|--------------|-----|
| `-o DIR` | - | каталог снимка, создаётся |
| `-ceph CMD` | `ceph` | как вызывать ceph: `"sudo ceph"`, `"cephadm shell -- ceph"` |
| `-name NAME` | - | имя кластера для отчёта |

Выполняются **только команды чтения**:

| Файл | Команда | Зачем |
|------|---------|-------|
| `osdmap.bin` | `ceph osd getmap` | бинарная osdmap для `verify` (через stdout, работает и в `cephadm shell`) |
| `osd_dump.json` | `ceph osd dump -f json` | пулы, OSD up/in/reweight, `pg_upmap`, `pg_upmap_items`, ratio |
| `osd_tree.json` | `ceph osd tree -f json` | дерево хост/стойка/ряд для имён в `-target` и отчёта |
| `crush.json` | `ceph osd crush dump -f json` | CRUSH-карта: бакеты, веса, правила, tunables, choose_args |
| `osd_df.json` | `ceph osd df -f json` | размер и занятость каждого OSD, класс |
| `pg_ls.json` | `ceph pg ls -f json` | up и acting каждой PG, байты и объекты |
| `status.json` | `ceph status -f json` | текущая скорость записи (`write_bytes_sec`) для догона после noout |
| `ec_profiles.json` | `ceph osd erasure-code-profile get <p>` | k и m для EC-пулов; у нас пустой `{}` |
| `meta.json` | `ceph config show osd.N ...`, `ceph config get mon mon_max_pg_per_osd` | время снимка, `osd_max_backfills`, mClock, `mon_max_pg_per_osd` |

`meta.json` снимается с одного OSD каждого класса. Ключи `osd_op_queue`, `osd_mclock_profile`, `osd_mclock_max_capacity_iops_{hdd,ssd}` и `osd_mclock_max_sequential_bandwidth_{hdd,ssd}` описывают mClock, для профиля `custom` дополнительно снимаются резервы, веса и лимиты (`osd_mclock_scheduler_*`).

Размер снимка определяет в основном `pg_ls.json`: на 4,5 тыс. PG - около 11 MB, на 20 тыс. PG - около 50 MB. Снимок отражает момент съёмки: после крупных изменений в кластере снимайте заново.

### Снимок без cephsim на кластере

Если запускать cephsim на кластере нельзя, снимок снимается с рабочей машины по ssh: на кластере нужен только `sudo ceph`.

```bash
SNAP=snap-prod; H=admin@ceph-mon-01; mkdir -p $SNAP
r() { ssh $H "sudo -n ceph $* 2>/dev/null"; }
# бинарная osdmap через base64, чтобы не испортить байты в терминале
ssh $H 'sudo -n ceph osd getmap 2>/dev/null | base64 -w0' | base64 -d > $SNAP/osdmap.bin
for x in "osd_dump.json:osd dump -f json" "osd_tree.json:osd tree -f json" \
         "crush.json:osd crush dump -f json" "osd_df.json:osd df -f json" \
         "pg_ls.json:pg ls -f json" "status.json:status -f json"; do
  r ${x#*:} > $SNAP/${x%%:*}
done
echo '{}' > $SNAP/ec_profiles.json   # если есть EC-пулы: ceph osd erasure-code-profile get <p> -f json по каждому профилю
```

`meta.json` в этом случае пишется вручную по выводу `ceph config show`:

```bash
ssh $H 'for k in osd_max_backfills osd_op_queue osd_mclock_profile \
  osd_mclock_max_capacity_iops_hdd osd_mclock_max_sequential_bandwidth_hdd; do
  echo $k $(sudo -n ceph config show osd.<HDD-OSD> $k); done
  for k in mon_max_pg_per_osd mon_osd_down_out_subtree_limit mon_osd_min_in_ratio; do
  echo $k $(sudo -n ceph config get mon $k); done'
```

```json
{
  "collected_at": "2026-10-27 12:17",
  "cluster": "PROD",
  "osd_max_backfills": 1,
  "osd_mclock_profile": "high_client_ops",
  "mclock": {
    "osd_op_queue": "mclock_scheduler",
    "profile": "high_client_ops",
    "capacity_iops": {"hdd": 8000, "ssd": 60000},
    "capacity_bytes_s": {}
  },
  "mon_max_pg_per_osd": 3000,
  "mon_osd_down_out_subtree_limit": "rack",
  "mon_osd_min_in_ratio": 0.75
}
```

`capacity_bytes_s` пуст, если параметра `osd_mclock_max_sequential_bandwidth_*` нет: в Quincy 17.2.6 и раньше его нет, и ограничение mClock тогда не моделируется (см. [calibration.md](calibration.md#mclock)).

## verify

Сверяет Go-раскладку со штатными инструментами Ceph. Делайте один раз на каждый новый кластер и после обновления Ceph. Если сверка не сошлась, simulate на этом кластере неточен.

| Флаг | По умолчанию | Что |
|------|--------------|-----|
| `-snap DIR` | - | снимок |
| `-image` | `quay.io/ceph/ceph:v20` | образ для `osdmaptool` через docker |
| `-ceph-bin DIR` | - | каталог с локальными `osdmaptool` и `crushtool` |
| `-out-osd N` | первый существующий | OSD для проверки out |

Три сверки по всем PG всех пулов:

1. Текущая раскладка против `osdmaptool --test-map-pgs-dump-all`.
2. Один OSD в out против `osdmaptool --mark-out N`.
3. Текущая раскладка против up из `pg_ls.json` (для реальных снимков). Это главная проверка: так кластер раскладывает PG на самом деле.

Сравниваются **up**-наборы. `osdmaptool --test-map-pgs-dump` (без `-all`) печатает acting с учётом `pg_temp`, а во время backfill он отличается от up. Из-за этого на кластере с идущим backfill первая версия verify показывала сотни ложных расхождений.

```
  ✔ текущая раскладка vs osdmaptool    19649 PG совпали
  ✔ out osd.0 vs osdmaptool --mark-out 19649 PG совпали
  ✔ текущая раскладка vs up из pg ls   19649 PG совпали
```

Образ Ceph может быть новее кластера: формат osdmap обратно совместим. Кластер на Quincy 17.2.6 сверялся образом v20.

## calibrate

Считает по истории Prometheus или VictoriaMetrics скорость одного backfill-слота, предел объектов в секунду и клиентскую нагрузку на OSD. Результат записывается в `DIR/calibration.json`, и `simulate` подхватывает его сам. Подробно о методе - в [calibration.md](calibration.md).

| Флаг | По умолчанию | Что |
|------|--------------|-----|
| `-snap DIR` | - | снимок, куда писать `calibration.json` |
| `-prometheus URL` | - | API Prometheus; для кластерной VictoriaMetrics - `http://victoria-metrics:8428/select/0/prometheus` |
| `-selector` | - | доп. матчеры, обязательно, если в хранилище метрик несколько кластеров: `'cluster="PROD"'` |
| `-since` | `30d` | глубина истории (`30d`, `72h`) |
| `-step` | `5m` | шаг выборки и окно `rate()`; растёт сам, если точек больше 10 000 |
| `-source` | `auto` | `osd` - per-OSD счётчик, `pool` - метрики пулов, `auto` - сначала osd, потом pool |
| `-metric` | `ceph_osd_recovery_bytes` | счётчик байт recovery на OSD |
| `-backfills` | `1` | `osd_max_backfills`, действовавший в этот период |
| `-percentile` | `75` | какой перцентиль скорости занятого OSD брать |
| `-min-mbps` | `1` | ниже - OSD или пул не считается занятым recovery |

Переменная `CEPHSIM_PROM_TOKEN` - bearer-токен, если API требует авторизации.

```
Калибровка (osd) по http://victoria-metrics:8428/select/0/prometheus за 30d
  класс   замеров    OSD   p50 MB/s   p90 MB/s      слот MB/s
  hdd       33661    372        5.2       47.2           35.2
  класс   замеров  пулов   p50 об/с   p90 об/с      слот об/с
  hdd       10894    110       16.5       30.2           24.0
  клиенты на OSD: ...
```

Обращайте внимание на «замеров»: класс с единицами замеров откалиброван ненадёжно. Например, у SSD 2 замера, если recovery на SSD в этот период почти не было. Тогда задайте скорость явно через `simulate -slot-mbps`.

## report

Сводный отчёт по кластеру - «карта рисков». Отвечает сразу на типовые вопросы перед любым окном работ, без перебора сценариев вручную.

```bash
cephsim report -snap snap-prod > prod-cluster.html
cephsim report -snap snap-prod -window 1h -format json > prod-cluster.json
```

| Флаг | По умолчанию | Что |
|------|--------------|-----|
| `-snap DIR` | - | снимок |
| `-window` | `2h` | окно обслуживания одного хоста или партии |
| `-gap` | `15m` | пауза между партиями в расписании |
| `-backfills` | из `meta.json`, иначе 2 | `osd_max_backfills` |
| `-client-load` | `p95` | клиентская нагрузка из калибровки |
| `-max-pg-per-osd` | из `meta.json`, иначе 250 | `mon_max_pg_per_osd` |
| `-format` | `html` | `html` или `json` |

Что считается для **каждого** бакета CRUSH-дерева от хоста и выше (хосты, стойки, ряды; корень - нет):

| Проверка | Как | Что показывает |
|----------|-----|----------------|
| обслуживание с noout | maintenance, только доступность | сколько PG станут недоступны и сколько останутся без запаса |
| внезапный отказ | failure с полной перебалансировкой | недоступные PG, заполненность после, переполнение, переезд и время |
| сейчас | maintenance по acting из `pg ls` | сколько PG станут недоступны, если выключить прямо сейчас, пока идёт recovery |

Первые две проверки делаются для здорового кластера: по up-наборам, как после HEALTH_OK. Так они показывают риски конструкции - правил, размещения, места. Колонка «сейчас» показывает отдельно, что блокирует текущая деградация.

Плюс:
- расписание обслуживания всех хостов (как `simulate -action rolling -assume-clean`);
- заполненность OSD и запас до nearfull: сколько сырого места можно занять, пока первый OSD не дойдёт до nearfull, если всё растёт пропорционально текущему;
- пулы: группы по типу и `min_size`, 10 самых больших;
- состояние кластера из `ceph status`.

Отчёт рассчитан на человека, который не знает Ceph изнутри. Сверху вниз:

1. **Коротко** - два-три предложения простым языком: в каком состоянии кластер, что можно делать сейчас, что главное запланировать.
2. **Ответы на частые вопросы** со светофором: можно ли прямо сейчас выключить хост; переживёт ли кластер отказ хоста, стойки, ряда; хватает ли места; сколько займёт обслуживание всех хостов; насколько надёжно хранятся данные.
3. **Риски по срочности**: «Сделать сейчас» (текущее состояние: восстановление, флаги, health), «Запланировать» (конструкция: правила, размещение, пороги, пулы), «Хорошие новости» (например, обслуживание партиями). Каждый риск - карточка: что не так, чем грозит (с цифрами и именами), что сделать; команды для инженера свёрнуты под кнопкой.
4. Ключевые цифры, карта отказов и план обслуживания.
5. Свёрнутые подробности для инженеров: таблицы по каждому ряду, стойке и хосту, заполненность дисков, пулы.
6. **Словарь**: OSD, PG, noout, nearfull, backfillfull, min_size, HEALTH_OK, out. В тексте рисков эти термины подчёркнуты пунктиром и объясняются при наведении.

Какие риски распознаются:

| Риск | Когда |
|------|-------|
| отказ одного домена делает данные недоступными | при отказе хоста, стойки или ряда есть inactive или incomplete PG |
| потеря домена переполнит кластер | после переезда OSD выше backfillfull или full; если тип домена не ниже `mon_osd_down_out_subtree_limit`, Ceph сам OSD не выведет - тогда это предупреждение «не выводить вручную» |
| домен нельзя выключить даже на обслуживание | недоступные PG при noout в здоровом кластере |
| идёт восстановление | в снимке есть PG без полного числа копий; называются хосты, которые можно выключать прямо сейчас |
| OSD выше nearfull; backfillfull не ниже full | по порогам из osdmap |
| пулы с `min_size 1` | у replicated-пулов |
| флаги `noout`, `norebalance`, `nobackfill`, `norecover`, `noscrub`, `nodeep-scrub`, `pause` | по `flags` в osdmap снимка |
| кластер не в HEALTH_OK | проверки из `ceph status` с расшифровкой частых кодов |
| слишком много PG на OSD | максимум выше `mon_max_pg_per_osd` |

`collect` снимает `mon_osd_down_out_subtree_limit` и `mon_osd_min_in_ratio` в `meta.json`.

Разделы HTML после рисков: ключевые цифры, «Карта отказов» (дерево, цвет хоста - худшее из обслуживания и отказа, рамка стойки или ряда - что будет при потере всего домена), таблицы по уровням (ряды, стойки, хосты; опасные сверху), расписание (диаграмма Ганта), заполненность OSD, пулы.

Время: 5 доменов и 4,5 тыс. PG - 0,15 с, 61 домен и 19,6 тыс. PG - около 5 с.

## simulate

```
cephsim simulate -snap DIR -action ACTION [флаги]
```

### Общие флаги

| Флаг | По умолчанию | Что |
|------|--------------|-----|
| `-snap DIR` | - | снимок |
| `-action` | `maintenance` | `maintenance`, `failure`, `drain`, `expand`, `rule`, `pgnum`, `rolling` |
| `-target` | - | имена из `ceph osd tree` через запятую: `host-07`, `rack2`, `row2`, `osd.12`. Обязателен для maintenance, failure, drain |
| `-backfills N` | из `meta.json`, иначе 2 | `osd_max_backfills` во время работ |
| `-slot-mbps` | калибровка, иначе `hdd=40,ssd=150,nvme=300` | скорость одного слота по классам, MB/s |
| `-slot-ops` | калибровка, иначе без предела | объектов/с на слот по классам |
| `-client-load` | `p95` | клиентская нагрузка из калибровки: `p50`, `p95`, `p99`, `recovery` (среднее во время прошлых recovery), `none` |
| `-client-ops`, `-client-mbps` | - | нагрузка на один OSD вручную, для всех классов; отменяет `-client-load` |
| `-assume-clean` | выкл. | доступность по up-наборам, то есть как после завершения текущего recovery или backfill |
| `-max-pg-per-osd` | из `meta.json`, иначе 250 | `mon_max_pg_per_osd` |
| `-compare` | выкл. | сравнить варианты: noout или нет, `osd_max_backfills` 1/3/6 |
| `-format` | `text` | `text`, `json` или `html` |
| `-no-color` | выкл. | без ANSI-цветов (в пайпе цвет выключается сам) |

Приоритет скоростей: значения по умолчанию, поверх них `calibration.json`, поверх него флаги. Если задан `-slot-mbps`, отметка о калибровке в отчёте снимается.

### Флаги сценариев

| Флаг | Сценарий | Что |
|------|----------|-----|
| `-noout` | maintenance | данные не переносятся, пока хост выключен |
| `-window` | maintenance, rolling | длительность работ (на партию в rolling), по умолчанию `3h` |
| `-add SPEC` | expand | новый хост; флаг повторяется. Формат - в [scenarios.md](scenarios.md#expand) |
| `-pool` | rule, pgnum | имя или id пула |
| `-rule` | rule | существующее CRUSH-правило |
| `-new-rule SPEC` | rule | новое правило. Формат - в [scenarios.md](scenarios.md#rule) |
| `-pg-num N` | pgnum | новый `pg_num`; `pgp_num` станет таким же |
| `-parallel N` | rolling | не больше N хостов в партии; 0 - сколько позволяет доступность |
| `-gap` | rolling | пауза между партиями, по умолчанию `15m` |

### HTML-отчёт

`-format html` пишет в stdout один самодостаточный HTML-файл: без интернета, внешних скриптов и шрифтов, со светлой и тёмной темой, читается и с телефона. Его можно приложить к заявке на работы или переслать.

```bash
cephsim simulate -snap snap-prod -action drain -target host-07 -compare -format html > drain-host-07.html
cephsim simulate -snap snap-prod -action rolling -window 2h -assume-clean -format html > rolling.html
```

Разделы отчёта сверху вниз:

| Раздел | Что показывает |
|--------|----------------|
| вердикт | цветная плашка «безопасно / можно, с условиями / нельзя» и причины |
| главные цифры | недоступно PG, без запаса, переедет, время, максимальная заполненность OSD; цвет полосы - уровень риска |
| Кластер | дерево CRUSH до хостов: выключаемые и выводимые хосты, новые хосты, у каждого - максимальная заполненность OSD после работ |
| Заполненность OSD | столбик на каждый OSD после работ, серая черта - сейчас, пунктир - nearfull, backfillfull, full; новые OSD обведены |
| Ход переезда | оценка доли ещё не переехавших данных во времени и отметка «90% переехало за …»; под графиком - узкое место |
| Клиенты (mClock) | сколько ёмкости OSD нужно клиентам, сколько даёт профиль, сколько остаётся recovery |
| Варианты | при `-compare`: переезд, время, итог; рекомендуемый выделен |
| Пулы | сначала проблемные; первые 12, остальные под «ещё N пулов» |
| План | шаги; команды в блоках с кнопкой «копировать» |
| Откуда данные | снимок, калибровка или значения по умолчанию, нагрузка, напоминание о `verify` |

Для `rolling` вместо дерева и графиков заполненности - диаграмма Ганта по партиям (окно и догон записей), таблица партий, хосты, которые нельзя выключать, с причиной, и план по партиям.

Кривая хода переезда - оценка: каждый OSD равномерно принимает и отдаёт свои данные за своё время, берётся худшая из сторон. Конечная точка совпадает с временем recovery модели, форма кривой - приближение.

### JSON-вывод

`-format json` выдаёт ту же информацию, что текстовый отчёт. Длительности (`recovery_time`, `catchup_time`, `window`, `time`, `start`, `gap`, `total_time`) - в **наносекундах** (Go `time.Duration`). Доли заполненности - от 0 до 1.

Сценарии кроме rolling:

| Поле | Что |
|------|-----|
| `scenario` | входные параметры, включая скорости и нагрузку, с которыми считалось |
| `verdict` | `ok`, `warn`, `bad` |
| `reasons[]` | `{level, text}` - почему такой вердикт |
| `plan[]` | команды и шаги |
| `pg_degraded`, `pg_inactive`, `pg_incomplete`, `pg_at_risk` | доступность |
| `pg_undersized_now` | PG, у которых acting неполный уже в снимке |
| `pg_lost_already_undersized` | из inactive/incomplete - те, что были неполными ещё до сценария |
| `moved_bytes`, `moved_objects`, `moved_pgs` | переезд (байты и объекты считаются по каждой новой копии) |
| `recovery_time`, `recovery_stalled` | время до конца переезда; stalled - упрётся в backfillfull |
| `bottleneck` | OSD, определяющий время: `in_bytes`, `out_bytes`, `in_objects`, `out_objects`, `limit` (`bytes`, `objects`, `mclock`) |
| `catchup_bytes`, `catchup_time` | догон записей после maintenance с noout |
| `max_osd_before`, `max_osd_after`, `top_osds[]` | заполненность OSD до и после |
| `nearfull_before`, `nearfull_after`, `backfillfull_after`, `full_after` | сколько OSD выше порогов |
| `ratios_near_backfill_full` | пороги кластера |
| `pg_per_osd` | `before_avg`, `before_max`, `after_avg`, `after_max`, `after_max_osd` |
| `pg_undersized_forever`, `pg_upmap_stuck` | PG без полного набора после переезда; PG, чьи upmap всё ещё указывают на выводимые OSD |
| `added_osds`, `added_fill_min_max`, `change` | expand, rule, pgnum: новые OSD, их заполненность после, описание изменения |
| `class_moves` | класс → `[байт, объектов]`, принятых OSD этого класса |
| `clients` | класс → доли ёмкости mClock: `demand_share`, `client_cap_share`, `recovery_share`, `clients_throttled` |
| `pools[]` | по пулам: PG, degraded, inactive, без запаса, remapped, undersized |
| `variants[]` | при `-compare`: `name`, `noout`, `backfills`, `moved_bytes`, `time`, `verdict`, `best` |

rolling:

| Поле | Что |
|------|-----|
| `batches[]` | `hosts`, `osds`, `pg_degraded`, `pg_at_risk`, `catchup_bytes`, `catchup_time`, `start`, `time` |
| `blocked[]` | хосты, которые нельзя выключить даже поодиночке: `pg_inactive`, `pg_incomplete`, `pg_already_undersized` |
| `total_time`, `max_parallel`, `gap`, `pg_undersized_now` | итог и параметры |
| `verdict`, `reasons`, `plan` | как выше |
