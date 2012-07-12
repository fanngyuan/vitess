# Copyright 2012, Google Inc. All rights reserved.
# Use of this source code is governed by a BSD-style license that can
# be found in the LICENSE file.

"""times module

This module provides some Date and Time interface for vtdb

Use Python datetime module to handle date and time columns."""

from datetime import date, datetime, time, timedelta
from math import modf
from time import localtime

# FIXME(msolomon) what are these aliasesf for?
Date = date
Time = time
TimeDelta = timedelta
Timestamp = datetime

DateTimeDeltaType = timedelta
DateTimeType = datetime

def DateFromTicks(ticks):
  """Convert UNIX ticks into a date instance."""
  return date(*localtime(ticks)[:3])

def TimeFromTicks(ticks):
  """Convert UNIX ticks into a time instance."""
  return time(*localtime(ticks)[3:6])

def TimestampFromTicks(ticks):
  """Convert UNIX ticks into a datetime instance."""
  return datetime(*localtime(ticks)[:6])

def DateTimeOrNone(s):
  if ' ' in s:
    sep = ' '
  elif 'T' in s:
    sep = 'T'
  else:
    return DateOrNone(s)

  try:
    d, t = s.split(sep, 1)
    return datetime(*[ int(x) for x in d.split('-')+t.split(':') ])
  except:
    return DateOrNone(s)

def TimeDeltaOrNone(s):
  try:
    h, m, s = s.split(':')
    td = timedelta(hours=int(h), minutes=int(m), seconds=int(float(s)), microseconds=int(modf(float(s))[0]*1000000))
    if h < 0:
      return -td
    else:
      return td
  except:
    return None

def TimeOrNone(s):
  try:
    h, m, s = s.split(':')
    return time(hour=int(h), minute=int(m), second=int(float(s)), microsecond=int(modf(float(s))[0]*1000000))
  except:
    return None

def DateOrNone(s):
  try: return date(*[ int(x) for x in s.split('-',2)])
  except: return None

def DateToString(d):
  return d.strftime("%Y-%m-%d")

def DateTimeToString(dt):
  return dt.strftime("%Y-%m-%d %H:%M:%S")
