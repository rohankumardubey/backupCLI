/*!40101 SET NAMES binary*/;
DROP TABLE IF EXISTS `v1`;
DROP VIEW IF EXISTS `v1`;
SET @PREV_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT;
SET @PREV_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS;
SET @PREV_COLLATION_CONNECTION=@@COLLATION_CONNECTION;
SET character_set_client = utf8;
SET character_set_results = utf8;
SET collation_connection = utf8_general_ci;
CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`192.168.198.178` SQL SECURITY DEFINER VIEW `v1` (`i`, `s`) AS SELECT `i`,`s` FROM `db1`.`tbl`;
SET character_set_client = @PREV_CHARACTER_SET_CLIENT;
SET character_set_results = @PREV_CHARACTER_SET_RESULTS;
SET collation_connection = @PREV_COLLATION_CONNECTION;
