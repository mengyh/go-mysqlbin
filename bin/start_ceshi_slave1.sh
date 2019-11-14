#!/bin/sh
nohup /opt/go_mysqlbin/src/bin/go-mysqlbin  -config=/opt/go_mysqlbin/src/etc/river.toml >>/opt/go_mysqlbin/src/slave1.log 2>&1 &
