---
title: "Использование"
description: "Развёртывание кластеров Ceph с помощью модуля sds-elastic: включение сопутствующих модулей, выбор бэкенд-хранилища OSD и практические примеры."
weight: 50
---

## Включение нужных модулей

{{< alert level="warning" >}}
Модуль `sds-elastic` находится в стадии `Experimental`. По умолчанию модули в этой стадии не включаются. Перед включением модуля установите `allowExperimentalModules: true` в ModuleConfig `deckhouse`.
{{< /alert >}}

Включите `sds-elastic` вместе со связанными модулями.
В примере ниже включаются `sds-node-configurator` (нужен для LVM-варианта),
`snapshot-controller` (нужен для снимков томов) и `csi-ceph` (нужен для `csiCephIntegration`):

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

## Выбор бэкенд-хранилища OSD

`sds-elastic` поддерживает два взаимоисключающих варианта бэкенд-хранилища OSD, выбираемых в `spec.storage`:

- `spec.storage.devices` — Rook напрямую использует сырые блочные устройства на каждом выбранном узле.
  Подходит, когда на каждом storage-узле уже есть выделенный пустой диск под Ceph.
- `spec.storage.lvm` — Rook использует LVM-тома, создаваемые модулем
  [sds-node-configurator](/modules/sds-node-configurator/) поверх заранее созданных объектов
  LVMVolumeGroup. Подходит, когда storage-узлы разделяют диски с другими нагрузками.

Должен быть задан ровно один из двух вариантов; CRD валидирует это ограничение.

## Пример: кластер на сырых блочных устройствах

В этом примере разворачивается Ceph-кластер с фактором репликации 3, который использует сырое
устройство `/dev/sdc` на каждом узле с лейблом `node-role.deckhouse.io/storage`, создаёт один
реплицированный CephBlockPool с именем `replicapool` и формирует объекты `csi-ceph`, чтобы рабочие
нагрузки могли потреблять пул через обычный StorageClass.

1. Промаркируйте узлы, на которых должны работать демоны Ceph:

   ```shell
   d8 k label node <имя-узла> node-role.deckhouse.io/storage=
   ```

1. Примените манифест кластера:

   ```shell
   d8 k apply -f - <<EOF
   apiVersion: storage.deckhouse.io/v1alpha1
   kind: SdsElasticCluster
   metadata:
     name: bare-devices
   spec:
     cephVersion: v19.2.3
     network:
       # CIDR публичной сети Ceph для трафика клиентов.
       public: 10.12.0.0/16
       # CIDR кластерной сети Ceph для репликации и heartbeat.
       cluster: 10.12.0.0/16
     storage:
       devices:
         useAllNodes: true
         useAllDevices: false
         # Регулярное выражение для путей устройств, отдаваемых под OSD.
         deviceFilter: "^/dev/sdc$"
     blockPools:
       - name: replicapool
         replicated:
           size: 3
           requireSafeReplicaSize: true
     csiCephIntegration:
       enabled: true
     placement:
       all:
         nodeAffinity:
           requiredDuringSchedulingIgnoredDuringExecution:
             nodeSelectorTerms:
               - matchExpressions:
                   - { key: node-role.deckhouse.io/storage, operator: Exists }
   EOF
   ```

1. Дождитесь, пока объект перейдёт в `Ready`:

   ```shell
   d8 k get sdselasticcluster bare-devices -w
   ```

   Ожидается, что значение в колонке `Phase` пройдёт путь `Pending` → `InProgress` → `Ready`.

1. Проверьте, что демоны Ceph запущены:

   ```shell
   d8 k -n d8-sds-elastic get pod -owide
   ```

1. Проверьте объекты `csi-ceph`, созданные интеграцией:

   ```shell
   d8 k get cephclusterconnection
   d8 k get cephstorageclass
   d8 k get sc
   ```

   В выводе должны присутствовать: CephClusterConnection с именем `ceph-cluster-connection`,
   CephStorageClass с именем `sds-elastic-rbd-replicapool` и одноимённый Kubernetes StorageClass,
   готовый к использованию в PersistentVolumeClaim. Имена формируются по шаблону
   `sds-elastic-rbd-<пул>` для блочных пулов и `sds-elastic-cephfs-<файловая-система>` для CephFS.

## Пример: кластер поверх LVM

В этом примере Ceph-кластер разворачивается на трёх узлах с именами `sds-elastic-test-5-ubuntu-0`,
`sds-elastic-test-5-ubuntu-1` и `sds-elastic-test-5-ubuntu-2`. На каждом узле должна уже
существовать volume group с именем `vg-ceph` с достаточным объёмом свободного места; ниже она
создаётся модулем [sds-node-configurator](/modules/sds-node-configurator/) из сырых дисков через
LVMVolumeGroupSet.

1. Создайте volume groups на целевых узлах:

   ```shell
   d8 k apply -f - <<EOF
   apiVersion: storage.deckhouse.io/v1alpha1
   kind: LVMVolumeGroupSet
   metadata:
     name: lvg-ceph
   spec:
     strategy: PerNode
     nodeSelector:
       matchExpressions:
         - key: kubernetes.io/hostname
           operator: In
           values:
             - sds-elastic-test-5-ubuntu-0
             - sds-elastic-test-5-ubuntu-1
             - sds-elastic-test-5-ubuntu-2
     lvmVolumeGroupTemplate:
       type: Local
       actualVGNameOnTheNode: vg-ceph
       metadata:
         labels:
           app: ceph-osd
       blockDeviceSelector:
         matchExpressions:
           - key: status.blockdevice.storage.deckhouse.io/type
             operator: In
             values:
               - "disk"
   EOF
   ```

   Дождитесь, пока LVMVolumeGroup на каждом узле перейдут в `Ready`:

   ```shell
   d8 k get lvmvolumegroup -l app=ceph-osd
   ```

1. Примените манифест кластера.
   Контроллер создаст в `vg-ceph` на каждом из трёх узлов LVMLogicalVolume размером 20 GiB с
   именем `lv-osd` и передаст полученные устройства Rook в качестве OSD:

   ```shell
   d8 k apply -f - <<EOF
   apiVersion: storage.deckhouse.io/v1alpha1
   kind: SdsElasticCluster
   metadata:
     name: ceph-lvm
   spec:
     cephVersion: v19.2.3
     network:
       public: 10.12.0.0/16
       cluster: 10.12.0.0/16
     mon:
       count: 3
     mgr:
       count: 3
     storage:
       lvm:
         # Префикс имён LVMVolumeGroup; группы на каждом узле сопоставляются как <lvgNamePrefix>-<i>.
         lvgNamePrefix: lvg-ceph
         # Существующая VG на узле (должна быть заранее создана).
         actualVGName: vg-ceph
         # Имя LV, создаваемого в каждой VG.
         actualLVName: lv-osd
         # Размер LV на каждом узле.
         lvSize: 20Gi
         # Префикс hostname; узлы сопоставляются как <nodeNamePrefix>-<i> через лейбл kubernetes.io/hostname.
         nodeNamePrefix: sds-elastic-test-5-ubuntu
         # Количество узлов/OSD, для которых создаются LV.
         nodeCount: 3
     blockPools:
       - name: replicapool
         failureDomain: host
         replicated:
           size: 3
   EOF
   ```

1. Дождитесь, пока объект перейдёт в `Ready`:

   ```shell
   d8 k get sdselasticcluster ceph-lvm -w
   ```

1. Проверьте, что демоны Ceph запущены и под OSD созданы нужные logical volumes:

   ```shell
   d8 k -n d8-sds-elastic get pod -owide
   d8 k get lvmlogicalvolume
   ```

## Удаление кластера

Чтобы удалить Ceph-кластер, удалите ресурс SdsElasticCluster:

```shell
d8 k delete sdselasticcluster <имя-кластера>
```

Контроллер выполняет удаление в обратном порядке:

1. Объекты CephStorageClass (csi-ceph).
1. CephClusterConnection (csi-ceph).
1. CephObjectStore, CephFilesystem, CephBlockPool (Rook).
1. Deployment `rook-ceph-tools` и CephCluster (Rook).
1. Локальные PV и объекты LVMLogicalVolume, созданные модулем.

На каждом шаге контроллер ждёт, пока соответствующий оператор (Rook или csi-ceph) выполнит свою часть очистки: освободит пулы, очистит OSD-устройства, удалит `cephx`-учётки и т. д. До завершения очистки на текущем шаге следующий шаг не запускается.

### Принудительное удаление

Если Ceph не вышел в кворум или Rook не может корректно удалить кластер, штатное удаление не произойдет, и SdsElasticCluster останется в состоянии `Terminating` неограниченно долго. Для таких случаев предусмотрено принудительное удаление.

{{< alert level="danger" >}}
Принудительное удаление снимает финализаторы с ресурсов Rook и csi-ceph в обход их штатной очистки. OSD-устройства не будут затёрты, учетные записи `cephx` не удалятся, остаточные данные на дисках придётся подчищать вручную. Используйте только как процедуру восстановления при проблемах с удалением.
{{< /alert >}}

Чтобы включить принудительное удаление, проставьте на SdsElasticCluster аннотацию `storage.deckhouse.io/force-delete` со значением `"true"`:

```shell
d8 k annotate sdselasticcluster <имя-кластера> storage.deckhouse.io/force-delete=true
```

После выставления аннотации контроллер ждёт grace-окно (по умолчанию 5 минут) с момента `deletionTimestamp`, затем снимает чужие финализаторы и завершает удаление.

## Отключение модуля

{{< alert level="danger" >}}
Отключение модуля останавливает контроллер и оператор Rook. Данные, хранящиеся в Ceph-кластерах под управлением модуля, могут стать недоступны или быть потеряны. Перед отключением модуля всегда удаляйте все объекты SdsElasticCluster.
{{< /alert >}}

1. Удалите все объекты SdsElasticCluster и дождитесь, пока контроллер полностью разберёт нижележащий Ceph-кластер:

   ```shell
   d8 k get sdselasticclusters.storage.deckhouse.io
   ```

   Дождитесь, пока команда вернёт `No resources found`.

1. Отключите модуль. Для отключения требуется лейбл `modules.deckhouse.io/allow-disabling: "true"` на ModuleConfig:

   ```shell
   d8 k label moduleconfig sds-elastic modules.deckhouse.io/allow-disabling=true --overwrite
   d8 k patch moduleconfig sds-elastic --type=merge -p '{"spec":{"enabled":false}}'
   ```

## Проверка работоспособности кластера

Посмотрите общее состояние [SdsElasticCluster](cr.html#sdselasticcluster) — контроллер выставляет ключевые условия (conditions) на нём:

```shell
d8 k describe sdselasticcluster <имя-кластера>
```

Полезные условия: `StorageReady`, `CephClusterReady`, `PoolsReady`, `FilesystemsReady`,
`ObjectStoresReady`, `CsiCephReady` и агрегирующее `Ready`.

Для более глубокой диагностики на уровне Ceph используйте toolbox-под, поставляемый Rook:

```shell
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph status
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph osd tree
```
