---
title: "Модуль sds-elastic"
description: "Модуль sds-elastic: распределённое блочное хранилище на базе оператора Rook Ceph."
weight: 1
---

{{< alert level="warning" >}}
Модуль находится в стадии `Experimental`. API, настройки и Custom Resources
могут меняться без предупреждения; не рассчитывайте на него в production-нагрузках.
{{< /alert >}}

Модуль `sds-elastic` устанавливает и сопровождает в кластере Deckhouse
Kubernetes [оператор Rook Ceph](https://rook.io), превращая набор узлов в
распределённый бэкенд блочного хранилища на основе Ceph.

Сейчас модуль представляет собой тонкую обёртку поверх апстрим-оператора:
поставляет Deployment оператора, ConfigMap `rook-ceph-operator-config` и
полный набор CRD Ceph. Развёртывание кластера, мониторинг, документация
по Custom Resources и end-to-end тесты вынесены за рамки версии v0.0.x и
будут добавлены в последующих релизах.
