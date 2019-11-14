#!/bin/sh
nohup /opt/go_mysqlbin/src/bin/go-mysqlbin  -config=/opt/go_mysqlbin/src/etc/river_rds_ceshi.toml >>/opt/go_mysqlbin/src/rds_slave1.log 2>&1 &
