---
title: "Использование"
description: "Развёртывание и управление Ceph-кластером через модуль sds-elastic: включение сопутствующих модулей, описание ElasticCluster + ElasticStorageClass и потребление получившегося StorageClass."
weight: 50
---

## Включение нужных модулей

{{< alert level="warning" >}}
Модуль `sds-elastic` находится в стадии `Experimental`. По умолчанию модули в этой стадии не включаются. Перед включением модуля установите `allowExperimentalModules: true` в ModuleConfig `deckhouse`.
{{< /alert >}}

Включите `sds-elastic` вместе со связанными модулями:

- [`sds-node-configurator`](/modules/sds-node-configurator/) — содержит CRD `BlockDevice` и `LVMVolumeGroup`, по которым ElasticCluster отбирает узлы и устройства.
- [`csi-ceph`](/modules/csi-ceph/) — содержит CRD `CephClusterConnection` и `CephStorageClass`, в которые пишет контроллер модуля.
- [`snapshot-controller`](/modules/snapshot-controller/) — нужен для поддержки VolumeSnapshot (опционально).

```shell
d8 k apply -f - <<EOF
apiVersion: v1
kind: List
items:
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: sds-node-configurator
    spec:
      enabled: true
      version: 1
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: snapshot-controller
    spec:
      enabled: true
      version: 1
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: csi-ceph
    spec:
      enabled: true
      version: 1
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: sds-elastic
    spec:
      enabled: true
      version: 1
EOF
```

Дождитесь, пока все модули перейдут в состояние `Ready`:

```shell
d8 k get module sds-node-configurator snapshot-controller csi-ceph sds-elastic -w
```

## Выбор data-узлов

`settings.dataNodes.nodeSelector` определяет, какие узлы Kubernetes пригодны для размещения данных sds-elastic. Контроллер проставляет лейбл `storage.deckhouse.io/sds-elastic-node=""` на каждый подходящий узел и снимает его с узлов, переставших соответствовать селектору.

Этот лейбл используют как nodeAffinity (правило сродства подов с узлами) downstream-компоненты: агент модуля [`sds-node-configurator`](/modules/sds-node-configurator/) (он выполняет discovery `BlockDevice` именно на data-узлах) и ваш `ElasticCluster.spec.storage.nodeSelector`.

```yaml
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: sds-elastic
spec:
  enabled: true
  version: 1
  settings:
    dataNodes:
      nodeSelector:
        node-role.deckhouse.io/storage: ""
```

Если параметр опущен, пустой селектор соответствует всем узлам кластера — каждый узел получит лейбл `storage.deckhouse.io/sds-elastic-node=""`.

{{< alert level="warning" >}}
Сужение `dataNodes.nodeSelector` не приводит к перераспределению данных. Если узел с уже размещёнными OSD перестал соответствовать новому селектору, с него будет снят лейбл `storage.deckhouse.io/sds-elastic-node`, и данные на нём станут недоступны до возвращения узла под селектор.
{{< /alert >}}

## Подготовка storage-узлов

`ElasticCluster` отбирает `BlockDevice` CR (управляются `sds-node-configurator`) по меткам и создаёт по одному OSD на каждое подходящее устройство.

1. Промаркируйте узлы, на которых должны работать демоны Ceph. В примере используется `node-role.deckhouse.io/storage`:

   ```shell
   d8 k label node <имя-узла> node-role.deckhouse.io/storage=
   ```

1. Убедитесь, что на каждом storage-узле есть хотя бы одно неиспользуемое сырое блочное устройство (без партиций, файловой системы и LVM-сигнатур). `sds-node-configurator` обнаружит его и создаст `BlockDevice` CR:

   ```shell
   d8 k get blockdevices.storage.deckhouse.io -o wide
   ```

1. Поставьте `BlockDevice`-объектам метку, по которой ElasticCluster будет отбирать устройства под OSD. В примере используется `app=elastic-osd`:

   ```shell
   d8 k label blockdevice <имя-bd> app=elastic-osd
   ```

## Развёртывание ElasticCluster

Пример ниже разворачивает Ceph-кластер на всех узлах с лейблом `node-role.deckhouse.io/storage` и использует все `BlockDevice` с лейблом `app=elastic-osd`.

```shell
d8 k apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: ElasticCluster
metadata:
  name: ceph-prod
spec:
  storage:
    nodeSelector:
      matchExpressions:
        - { key: node-role.deckhouse.io/storage, operator: Exists }
    blockDeviceSelector:
      matchLabels:
        app: elastic-osd
  network:
    public: 10.12.0.0/16
    cluster: 10.12.0.0/16
EOF
```

Дождитесь, пока `ElasticCluster` перейдёт в `Ready`:

```shell
d8 k get elasticcluster ceph-prod -w
```

Ожидается, что значение в колонке `Phase` пройдёт путь `Pending` → `InProgress` → `Ready`. Полный прогресс по этапам — через condition'ы: `StorageReady` → `CephClusterReady` → `CredentialsReady` → `CsiCephReady` → агрегирующий `Ready`.

Проверьте подкладку:

```shell
d8 k get lvmvolumegroup -l sds-elastic.deckhouse.io/cluster=ceph-prod
d8 k get lvmlogicalvolume -l sds-elastic.deckhouse.io/cluster=ceph-prod
d8 k get pv -l sds-elastic.deckhouse.io/cluster=ceph-prod
d8 k -n d8-sds-elastic get pod -owide
```

Контроллер также создаёт внутренний [ElasticClusterCredential](./cr.html#elasticclustercredential), зеркалирующий поля Secret'а `rook-ceph-mon`:

```shell
d8 k get elasticclustercredential ceph-prod -o yaml
```

## Принадлежность BlockDevice и правила усыновления

Как только `ElasticCluster` впервые выбирает `BlockDevice`, контроллер навешивает на него лейбл `sds-elastic.deckhouse.io/cluster=<имя-кластера>`. Это устойчивая отметка владельца, и она определяет несколько важных особенностей поведения:

- **Один BlockDevice — один владелец.** Если `BlockDevice` подходит под `blockDeviceSelector` сразу двух `ElasticCluster`, второй из них не сможет его усыновить. Контроллер отказывается перетирать чужой лейбл и поднимает `StorageReady=False` с `Reason=OwnershipConflict`; в `message` перечислены конфликтующие BD и их текущие владельцы. Пока конфликт не разрешён, ни `LVMVolumeGroup`, ни `LVMLogicalVolume`, ни локальные `PersistentVolume` не создаются — даже свободные BD из селектора остаются неусыновлёнными.

  Чтобы разрешить конфликт, выберите, какой кластер должен владеть устройством, и снимите лейбл с обратной стороны:

  ```shell
  d8 k label blockdevice <имя-bd> sds-elastic.deckhouse.io/cluster-
  ```

  Альтернативно — удалите конфликтующий `ElasticCluster`. Следующая реконсиляция подхватит BD.

- **Sticky-усыновление: усыновлённый BlockDevice остаётся за кластером.** Если контроллер уже навесил лейбл на BD, тот остаётся в составе кластера, даже если впоследствии перестанет соответствовать `blockDeviceSelector` или `nodeSelector` (например, оператор сузил селектор, изменились лейблы устройства, или ноду переразметили). Это сделано намеренно: OSD поверх такого BD уже создан, локальный PV привязан к конкретной ноде, а исключение его из рабочего набора уменьшит `CephCluster.spec.storageClassDeviceSets[0].count` и грозит недоступностью данных. Поэтому количество OSD у `ElasticCluster` монотонно: растёт при появлении новых подходящих BD и не уменьшается само по себе.

  Побочный эффект: `sds-node-configurator` после установки VG на устройство переключает `BlockDevice.status.consumable` в `false`. Sticky-логика не даёт этому факту выкинуть BD из рабочего набора на следующей реконсиляции.

- **Освобождение BlockDevice.** На текущей экспериментальной стадии автоматического «отказа от» BD не предусмотрено (запланировано в составе задачи B20 — OwnerReferences и финализаторы для каскадного удаления). Удаление `ElasticCluster` НЕ удаляет каскадно объекты per-device: контроллер сносит только Rook `CephCluster` и csi-ceph `CephClusterConnection`, а `LVMVolumeGroup` / `LVMLogicalVolume` / локальные `PersistentVolume` и лейбл на BD остаются — их нужно вычистить вручную по лейблу (см. раздел «Удаление ресурсов»). Чтобы вывести одно устройство из работающего кластера, вручную удалите соответствующие `LVMLogicalVolume` и `LVMVolumeGroup`, и только затем снимите лейбл:

  ```shell
  d8 k delete lvmlogicalvolume <имя>
  d8 k delete lvmvolumegroup <имя>
  d8 k label blockdevice <имя-bd> sds-elastic.deckhouse.io/cluster-
  ```

  Делать это, пока в пулах ещё хранятся ценные данные, рискованно: можно потерять реплики.

- **Редактирование селекторов после создания.** `ElasticCluster.spec.storage.nodeSelector` и `spec.storage.blockDeviceSelector` редактируются после создания — `kubectl edit elasticcluster <имя>` и поправьте матчеры. Validating-вебхук на UPDATE контролирует две инвариантные ловушки:

  - **Защита от orphan-эффектов.** Если правка выкидывает уже усыновлённый BD из новой пары селекторов (его лейблы больше не подходят `blockDeviceSelector`, или его `status.nodeName` больше не входит в множество, выдаваемое `nodeSelector`), вебхук отклоняет запрос и перечисляет проблемные BD. Усыновлённые BD не выводятся автоматически — сначала пройдите ручной процедурой выше.
  - **Pre-flight ownership-check.** Если расширение селектора подтянет BD, уже принадлежащий другому `ElasticCluster`, вебхук отклоняет запрос и перечисляет спорные BD вместе с их текущими владельцами. Сначала разрешите конфликт (снимите лейбл вручную или удалите чужой EC), затем повторите правку.

  `spec.network` остаётся неизменяемым на UPDATE: смена public/cluster CIDR на живом кластере инвалидирует endpoint'ы mon-ов и host-network bindings, безопасной автоматической миграции для этого нет. Чтобы поменять сетевую конфигурацию, удалите и пересоздайте `ElasticCluster`.

## Описание StorageClass'ов

Пулы и соответствующие им csi-ceph StorageClass'ы описываются по одному ресурсу [ElasticStorageClass](./cr.html#elasticstorageclass). Один ESC даёт один Ceph-пул и один `CephStorageClass` модуля csi-ceph с тем же именем.

### RBD-пул с дефолтной репликацией (3 реплики)

```shell
d8 k apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: ElasticStorageClass
metadata:
  name: ceph-prod-rbd
spec:
  clusterRef: ceph-prod
  type: RBD
  replication: ConsistencyAndAvailability
EOF
```

### CephFS-пул с дефолтной репликацией (3 реплики)

```shell
d8 k apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: ElasticStorageClass
metadata:
  name: ceph-prod-cephfs
spec:
  clusterRef: ceph-prod
  type: CephFS
  replication: ConsistencyAndAvailability
EOF
```

> Режим репликации `ErasureCodedCompact` временно отключён и недоступен для выбора.

### Пул, переживающий одновременный отказ двух хостов (`HighRedundancy`)

```shell
d8 k apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: ElasticStorageClass
metadata:
  name: ceph-prod-rbd-hr
spec:
  clusterRef: ceph-prod
  type: RBD
  replication: HighRedundancy
EOF
```

`HighRedundancy` создаёт пул с 4 репликами (`size=4`, `min_size=2`, `requireSafeReplicaSize=true`):

- два одновременных отказа хостов сохраняют непрерывный I/O (2 реплики совпадают с `min_size`);
- третий одновременный отказ останавливает I/O, но данные не теряются — Ceph в фоне восстанавливает выжившую копию на свободное пространство кластера и возобновляет работу;
- потеря данных только при четвёртом одновременном отказе.

Режим требует не менее **5 storage-узлов** (4 для CRUSH-размещения пула при `failureDomain=host` и 5 для кворума из 5 mon). При создании первого `HighRedundancy` ESC, ссылающегося на `ElasticCluster`, контроллер автоматически повышает топологию нижележащего `CephCluster` до `mon.count=5`, `mgr.count=3` (по умолчанию — `3, 2`). Повышение **залипающее**: удаление последнего `HighRedundancy` ESC НЕ возвращает счётчики обратно, поскольку молчаливое ослабление гарантии отказоустойчивости на живом кластере небезопасно.

Validating-вебхук закрывает создание ESC по тем же порогам, чтобы залипающее повышение не сработало на недостаточно крупном кластере. CREATE ESC с `replication: HighRedundancy` отклоняется, если:

- родительский `ElasticCluster`, указанный в `spec.clusterRef`, не существует;
- под `ElasticCluster.spec.storage.nodeSelector` подходит < 5 узлов (минимум для 5-mon-кворума);
- adopted `BlockDevice` родительского EC лежат на < 4 разных узлах (минимум для CRUSH-размещения 4 реплик).

Поэтому порядок bootstrap'а фиксирован: сначала применить `ElasticCluster`, дождаться, пока хотя бы на 4 узлах появятся adopted BD (`kubectl get bd -l sds-elastic.deckhouse.io/cluster=<ec>` или `EC.status.phase=Ready`), и только потом применять `HighRedundancy` ESC. Попытка отправить EC и HR-ESC в одном `kubectl apply` будет отклонена admission'ом: EC создастся первым, но его список adopted BD ещё пуст в момент admission ESC.

Аудит — в `ElasticCluster.status.cephTopology`:

```shell
d8 k get elasticcluster ceph-prod -o jsonpath='{.status.cephTopology}'
# {"monCount":5,"mgrCount":3,"reason":"HighRedundancyESCPresent","lastPromotedAt":"2026-…"}
```

Возможные значения `reason`: `Standard`, `HighRedundancyESCPresent`, `StickyHighWaterMark`. Чтобы пересчитать топологию принудительно (например, после сознательного даунскейла кластера), очистите поле через status-subresource и запустите реконсиль:

```shell
d8 k patch elasticcluster ceph-prod \
  --type=merge --subresource=status \
  -p '{"status":{"cephTopology":null}}'
```

Дождитесь, пока каждый ESC перейдёт в `Ready`:

```shell
d8 k get elasticstorageclass -w
```

Прогресс — `PoolReady` → `CsiStorageClassReady` → агрегирующий `Ready`.

Проверьте получившиеся csi-ceph-объекты и Kubernetes StorageClass'ы:

```shell
d8 k get cephclusterconnection
d8 k get cephstorageclass
d8 k get sc
```

Ожидается один `CephClusterConnection` с именем родительского `ElasticCluster` (`ceph-prod`) и по одному `CephStorageClass` на каждый `ElasticStorageClass` (`ceph-prod-rbd`, `ceph-prod-cephfs`). Каждый `CephStorageClass` создаёт одноимённый Kubernetes `StorageClass`, готовый к использованию в `PersistentVolumeClaim`.

Внутренний helm-managed `StorageClass` `sds-elastic-osd` (provisioner `kubernetes.io/no-provisioner`, `volumeBindingMode: WaitForFirstConsumer`) подкладывается под локальные OSD-PV и пользователю не предназначен — `ElasticStorageClass` с этим именем отклоняется validating-вебхуком.

## Удаление ресурсов

### Удаление ElasticCluster

Удаление `ElasticCluster` обратимо, пока на него не ссылается ни один `ElasticStorageClass`: контроллер удаляет только те ресурсы, которые нельзя удалить вручную, — Rook `CephCluster` и csi-ceph `CephClusterConnection` (оба защищены вебхуком vendor-cr-validation). Диски OSD и mon-хранилище остаются нетронутыми, поэтому кластер можно пересоздать на тех же устройствах.

Порядок действий:

1. Сначала удалите все зависимые `ElasticStorageClass` (см. ниже). Контроллер не начинает teardown кластера, пока на него ссылается хотя бы один ESC.
2. Удалите `ElasticCluster`:

   ```shell
   d8 k delete elasticcluster ceph-prod
   ```

   Удерживая объект финализатором, контроллер удаляет `CephCluster` и `CephClusterConnection`, после чего снимает финализатор.
3. Оставшиеся помеченные контроллером объекты вычистите вручную — они намеренно сохраняются (автоматического каскада нет):

   ```shell
   # посмотреть, что ещё помечено именем кластера
   d8 k get pv,lvmlogicalvolume,lvmvolumegroup -l sds-elastic.deckhouse.io/cluster=ceph-prod

   d8 k delete pv -l sds-elastic.deckhouse.io/cluster=ceph-prod
   d8 k delete lvmlogicalvolume -l sds-elastic.deckhouse.io/cluster=ceph-prod
   d8 k delete lvmvolumegroup -l sds-elastic.deckhouse.io/cluster=ceph-prod
   # в конце снять лейбл кластера с BlockDevice
   d8 k label blockdevice -l sds-elastic.deckhouse.io/cluster=ceph-prod sds-elastic.deckhouse.io/cluster-
   ```

   Сохраните `ElasticClusterCredential`, если планируете пересоздать кластер с той же идентичностью.

Пока идёт teardown, условие `Ready` у `ElasticCluster` объясняет, что его блокирует:

| Reason | Значение | Действие |
| --- | --- | --- |
| `StorageClassesExist` | На кластер ещё ссылается один или несколько `ElasticStorageClass`. | Сначала удалите перечисленные `ElasticStorageClass`. |
| `VolumesExist` | В backend ещё есть привязанные (bound) `PersistentVolume`. | Удалите оставшиеся `PersistentVolume` — teardown продолжится автоматически. |
| `Terminating` | Ресурсы backend удаляются. | Дождитесь завершения. |

### Удаление ElasticStorageClass

Удаление `ElasticStorageClass` уничтожает соответствующий пул и `CephStorageClass`:

```shell
d8 k delete elasticstorageclass ceph-prod-rbd
```

{{< alert level="warning" >}}
Удаление `ElasticStorageClass` — деструктивная операция: оно сносит нижележащий пул / файловую систему вместе с данными. Сначала убедитесь, что данные больше никому не нужны.
{{< /alert >}}

Удерживаемый финализатором, контроллер выполняет упорядоченный teardown:

1. Он не удаляет ничего, пока хотя бы один `PersistentVolume`, выданный этим StorageClass, остаётся в состоянии `Bound`. Сначала удалите потребляющие `PersistentVolumeClaim` — этот гард нельзя обойти.
2. Когда ничего не привязано, контроллер удаляет `CephStorageClass` и сносит нижележащий пул / файловую систему.

Для блочных (RBD) классов пул с данными по умолчанию сохраняется. Чтобы удалить его безвозвратно (данные в пуле будут потеряны), разрешите деструктивную очистку аннотацией force-deletion:

```shell
d8 k annotate elasticstorageclass ceph-prod-rbd sds-elastic.deckhouse.io/force-deletion=true
```

Для классов общей файловой системы (CephFS) force-режима нет: файловая система удаляется автоматически, как только становится пустой, — для этого удалите оставшиеся `PersistentVolume` этого StorageClass.

Пока идёт teardown, условие `Ready` ресурса `ElasticStorageClass` объясняет, что его блокирует:

| Reason | Значение | Действие |
| --- | --- | --- |
| `BoundVolumesExist` | `PersistentVolume`, выданные этим StorageClass, ещё привязаны. | Удалите потребляющие `PersistentVolumeClaim`. Аннотация force это не обходит. |
| `DataPresentInPool` | Блочный пул всё ещё содержит данные (только RBD). | Установите `sds-elastic.deckhouse.io/force-deletion=true`, чтобы безвозвратно удалить пул и его данные. |
| `FilesystemNotEmpty` | В файловой системе ещё есть тома (только CephFS). | Удалите оставшиеся `PersistentVolume` этого StorageClass. |
| `Terminating` | Ресурсы backend удаляются. | Дождитесь завершения. |

Зачистка PV / LVM / BlockDevice после удаления `ElasticCluster` выполняется вручную (см. выше); сквозной GC через OwnerReferences отслеживается как задача B20 в backlog.

## Отключение модуля

{{< alert level="danger" >}}
Отключение модуля останавливает контроллер и оператор Rook. Данные, хранящиеся в Ceph-кластерах под управлением модуля, могут стать недоступны или быть потеряны. Перед отключением модуля всегда удаляйте все объекты `ElasticCluster`, `ElasticStorageClass` и `ElasticClusterCredential`.
{{< /alert >}}

Валидирующий вебхук на `ModuleConfig` модуля `sds-elastic` **запрещает** установку `spec.enabled: false`, пока существует хотя бы один `ElasticCluster`. Это не даёт случайно остановить контроллер и оператор Rook, пока под управлением модуля ещё находится живой Ceph-кластер (данные OSD на дисках узлов). Выполняйте упорядоченное удаление ниже; отключение будет разрешено только после удаления последнего `ElasticCluster`.

1. Удалите все `ElasticStorageClass` и дождитесь, пока контроллер уберёт пулы и csi-ceph StorageClass'ы:

   ```shell
   d8 k get elasticstorageclasses.storage.deckhouse.io
   ```

   Дождитесь, пока команда вернёт `No resources found`.

1. Удалите все `ElasticCluster` и дождитесь teardown'а:

   ```shell
   d8 k get elasticclusters.storage.deckhouse.io
   ```

   Дождитесь, пока команда вернёт `No resources found`.

1. При необходимости удалите `ElasticClusterCredential`. Это cluster-scoped бэкап идентичности; на запрет отключения он не влияет (отключение блокирует только живой `ElasticCluster`). Удалите его, если не планируете пересоздавать кластер с той же идентичностью:

   ```shell
   d8 k get elasticclustercredentials.storage.deckhouse.io
   d8 k delete elasticclustercredential <имя>
   ```

1. Отключите модуль. Для отключения требуется аннотация `modules.deckhouse.io/allow-disabling: "true"` на ModuleConfig:

   ```shell
   d8 k annotate moduleconfig sds-elastic modules.deckhouse.io/allow-disabling=true --overwrite
   d8 k patch moduleconfig sds-elastic --type=merge -p '{"spec":{"enabled":false}}'
   ```

### Принудительное отключение модуля при оставшихся ElasticCluster

{{< alert level="danger" >}}
Это обходит защитную проверку. Используйте только при аварийном восстановлении, когда вы осознанно хотите сохранить объекты `ElasticCluster` и их данные на дисках, но прекратить управление ими со стороны модуля. Ceph-кластер останется без оператора (orphaned), а финализаторы контроллера на оставшихся CR будут сняты хуком удаления модуля, чтобы API-сервер мог их удалить. Данные OSD на дисках узлов и `dataDirHostPath` при этом **не** стираются, но больше не управляются и могут стать невосстановимыми штатными средствами.
{{< /alert >}}

Если необходимо отключить модуль без предварительного удаления `ElasticCluster`, установите на ModuleConfig аннотацию `sds-elastic.deckhouse.io/force-disable: "true"`. С этой аннотацией вебхук разрешает `spec.enabled: false` независимо от количества существующих `ElasticCluster`:

```shell
d8 k annotate moduleconfig sds-elastic sds-elastic.deckhouse.io/force-disable=true --overwrite
d8 k annotate moduleconfig sds-elastic modules.deckhouse.io/allow-disabling=true --overwrite
d8 k patch moduleconfig sds-elastic --type=merge -p '{"spec":{"enabled":false}}'
```

## Проверка работоспособности кластера

Контроллер выставляет общий прогресс на каждом CR через condition'ы. Для `ElasticCluster`:

```shell
d8 k describe elasticcluster <имя-кластера>
```

Полезные condition'ы: `StorageReady`, `CephClusterReady`, `CredentialsReady`, `CsiCephReady`, `UpgradeReady`, `UpgradeInProgress` и агрегирующий `Ready`.

Колонка `UPGRADING` (и стоящий за ней condition `UpgradeInProgress`) отслеживает гистограмму версий демонов, которую Rook публикует в `CephCluster.status.ceph.versions.overall`. Пока в map'е больше одного ключа — кластер находится в процессе раскатки, и `UPGRADING` остаётся `True` на всё время окна mon → mgr → osd → mds, в том числе когда `CephCluster.status.phase=Progressing` и FSM гейтит downstream-стейджи. `UpgradeInProgress` возвращается в `False` только когда в `versions.overall` остаётся единственный ключ, совпадающий с целевой версией. Поле `EC.status.cephVersion.running` (колонка `Ceph`) при наличии расхождения публикует **отстающую** версию из `versions.overall` — это та версия, на которую попадут запросы на самый медленный демон (обычно OSD), а не уже сменённый Rook'ом маркер целевой версии.

Для `ElasticStorageClass`:

```shell
d8 k describe elasticstorageclass <имя-esc>
```

Полезные condition'ы: `PoolReady`, `CsiStorageClassReady` и агрегирующий `Ready`.

Для глубокой диагностики на уровне Ceph используйте toolbox-под, поставляемый Rook:

```shell
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph status
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph osd tree
```
